package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/stackwright/stackwright/internal/oidc"
	"github.com/stackwright/stackwright/internal/seal"
	"github.com/stackwright/stackwright/internal/sts"
)

// AuditPhase is the phase in the subject of an audit export's token: a
// role that takes the export trusts
// org:<org id>:workspace::phase:audit-export.
const AuditPhase = "audit-export"

const (
	auditEvery = 5 * time.Minute
	auditBatch = 5000
)

// Auditor sends each org's audit rows to its S3 bucket and webhook, and
// deletes the rows past the org's retention that every export has
// (adr/0045).
type Auditor struct {
	Pool        *pgxpool.Pool
	KEK         seal.KEK
	Issuer      *oidc.Issuer
	STSEndpoint string
	// S3Endpoint replaces AWS's regional endpoint, for an S3-compatible
	// store or a test.
	S3Endpoint string
	Client     *http.Client
}

// Run exports and prunes every auditEvery until ctx ends.
func (a *Auditor) Run(ctx context.Context) {
	t := time.NewTicker(auditEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := a.ExportDue(ctx); err != nil && ctx.Err() == nil {
			slog.Error("export the audit log", "err", err)
		}
	}
}

// ExportDue exports and prunes the orgs that have audit work.
func (a *Auditor) ExportDue(ctx context.Context) error {
	rows, err := a.Pool.Query(ctx, `SELECT org_id FROM audit_work()`)
	if err != nil {
		return err
	}
	orgs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	var errs []error
	for _, org := range orgs {
		if err := pgx.BeginFunc(ctx, a.Pool, func(tx pgx.Tx) error { return a.export(ctx, tx, org) }); err != nil {
			errs = append(errs, fmt.Errorf("org %s: %w", org, err))
		}
	}
	return errors.Join(errs...)
}

type auditRow struct {
	ID     int64     `json:"id"`
	Actor  string    `json:"actor"`
	Kind   string    `json:"actor_kind"`
	Action string    `json:"action"`
	Target string    `json:"target"`
	IP     string    `json:"ip,omitempty"`
	At     time.Time `json:"at"`
}

func (a *Auditor) export(ctx context.Context, tx pgx.Tx, orgID string) error {
	if _, err := tx.Exec(ctx, `SELECT set_config('stackwright.org_id', $1, true)`, orgID); err != nil {
		return err
	}
	type target struct {
		kind, bucket, prefix string
		account              *string
		url, secret          []byte
		sent                 int64
	}
	rows, err := tx.Query(ctx, `SELECT kind, bucket, prefix, cloud_account_id::text, url, secret, sent_id FROM audit_exports WHERE org_id = $1`, orgID)
	if err != nil {
		return err
	}
	targets, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (target, error) {
		var t target
		return t, r.Scan(&t.kind, &t.bucket, &t.prefix, &t.account, &t.url, &t.secret, &t.sent)
	})
	if err != nil {
		return err
	}
	for _, t := range targets {
		rows, err := tx.Query(ctx, `SELECT id, actor_id, actor_kind, action, target, coalesce(host(ip), ''), at FROM audit_log
			WHERE org_id = $1 AND id > $2 ORDER BY id LIMIT $3`, orgID, t.sent, auditBatch)
		if err != nil {
			return err
		}
		batch, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (auditRow, error) {
			var x auditRow
			err := r.Scan(&x.ID, &x.Actor, &x.Kind, &x.Action, &x.Target, &x.IP, &x.At)
			x.At = x.At.UTC()
			return x, err
		})
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			continue
		}
		var body bytes.Buffer
		enc := json.NewEncoder(&body)
		for _, x := range batch {
			if err := enc.Encode(x); err != nil {
				return err
			}
		}
		first, last := batch[0].ID, batch[len(batch)-1].ID
		if t.kind == "s3" {
			err = a.putS3(ctx, tx, orgID, *t.account, t.bucket, fmt.Sprintf("%s%s/%d-%d.jsonl", t.prefix, batch[0].At.UTC().Format("2006/01/02"),
				first, last), body.Bytes())
		} else {
			err = a.postWebhook(ctx, tx, orgID, t.url, t.secret, body.Bytes())
		}
		if err != nil {
			if _, uerr := tx.Exec(ctx, `UPDATE audit_exports SET last_error = $3 WHERE org_id = $1 AND kind = $2`, orgID, t.kind, err.Error()); uerr != nil {
				return uerr
			}
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE audit_exports SET sent_id = $3, sent_at = now(), last_error = NULL WHERE org_id = $1 AND kind = $2`,
			orgID, t.kind, last); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `SELECT prune_audit_log($1)`, orgID)
	return err
}

// putS3 writes an object with the cloud account's apply role, assumed
// with a token for the audit export.
func (a *Auditor) putS3(ctx context.Context, tx pgx.Tx, orgID, accountID, bucket, key string, body []byte) error {
	var role, region, orgSlug string
	if err := tx.QueryRow(ctx, `SELECT a.aws_apply_role_arn, a.aws_region, o.slug FROM cloud_accounts a JOIN orgs o ON o.id = a.org_id
		WHERE a.id = $1`, accountID).Scan(&role, &region, &orgSlug); err != nil {
		return err
	}
	token, err := a.Issuer.Mint(oidc.Claims{OrgID: orgID, OrgSlug: orgSlug, Phase: AuditPhase}, oidc.AWSAudience)
	if err != nil {
		return err
	}
	creds, err := sts.Exchange(ctx, a.STSEndpoint, role, "stackwright-audit-export", token, 15*time.Minute)
	if err != nil {
		return err
	}
	endpoint, secure, lookup := "s3."+region+".amazonaws.com", true, minio.BucketLookupAuto
	if a.S3Endpoint != "" {
		u, err := url.Parse(a.S3Endpoint)
		if err != nil {
			return err
		}
		endpoint, secure, lookup = u.Host, u.Scheme == "https", minio.BucketLookupPath
	}
	client, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(creds.AccessKeyID, creds.SecretAccessKey, creds.SessionToken),
		Secure: secure, Region: region, BucketLookup: lookup})
	if err != nil {
		return err
	}
	_, err = client.PutObject(ctx, bucket, key, bytes.NewReader(body), int64(len(body)), minio.PutObjectOptions{ContentType: "application/x-ndjson"})
	return err
}

func (a *Auditor) postWebhook(ctx context.Context, tx pgx.Tx, orgID string, sealedURL, sealedSecret, body []byte) error {
	target, err := a.KEK.Open(ctx, tx, orgID, sealedURL)
	if err != nil {
		return err
	}
	secret, err := a.KEK.Open(ctx, tx, orgID, sealedSecret)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, string(target), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("User-Agent", "Stackwright")
	req.Header.Set("X-Stackwright-Event", "audit")
	req.Header.Set("X-Stackwright-Signature", Sign(secret, body))
	resp, err := a.Client.Do(req)
	if err != nil {
		return fmt.Errorf("post to %s: %w", req.URL.Host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("%s answered %s: %s", req.URL.Host, resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}
