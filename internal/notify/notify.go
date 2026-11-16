// Package notify sends run, finding and drift events to Slack and HTTPS
// webhooks through an outbox (adr/0045).
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/utibeabasi6/stackwright/internal/db"
	"github.com/utibeabasi6/stackwright/internal/seal"
)

// The events a destination can choose.
const (
	RunFailed        = "run_failed"
	RunNeedsApproval = "run_needs_approval"
	RunApplied       = "run_applied"
	FindingOpened    = "finding_opened"
	DriftFound       = "drift_found"
	// Test is what "Send a test" sends; every destination takes it.
	Test = "test"
)

// Event is what a delivery carries. Text is the line a person reads.
type Event struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Org       string `json:"org"`
	Project   string `json:"project,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Run       int32  `json:"run,omitempty"`
	// Deployment names a deployment, whose run is Run.
	Deployment string `json:"deployment,omitempty"`
	URL        string `json:"url,omitempty"`
	Error      string `json:"error,omitempty"`
	Resource   string `json:"resource,omitempty"`
	Severity   string `json:"severity,omitempty"`
	Check      string `json:"check,omitempty"`
}

// Enqueue writes a delivery of e for each of the org's destinations that
// chose its type, in the transaction that records what happened. tx needs
// the org setting.
func Enqueue(ctx context.Context, tx pgx.Tx, orgID string, e Event) error {
	payload, err := json.Marshal(e)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT id FROM notification_destinations WHERE $1 = ANY(events)`, e.Type)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `INSERT INTO notification_deliveries (id, org_id, destination_id, event, payload) VALUES ($1, $2, $3, $4, $5)`,
			uuid.NewString(), orgID, id, e.Type, payload); err != nil {
			return err
		}
	}
	return nil
}

// EnqueueTest writes a test delivery to one destination. tx needs the org
// setting.
func EnqueueTest(ctx context.Context, tx pgx.Tx, orgID, org, destinationID string) error {
	payload, err := json.Marshal(Event{Type: Test, Org: org, Text: "Stackwright can post here."})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO notification_deliveries (id, org_id, destination_id, event, payload) VALUES ($1, $2, $3, $4, $5)`,
		uuid.NewString(), orgID, destinationID, Test, payload)
	return err
}

// Sign is a webhook body's X-Stackwright-Signature.
func Sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Sender delivers the outbox.
type Sender struct {
	Pool      *pgxpool.Pool
	KEK       seal.KEK
	Client    *http.Client
	PublicURL string
}

const (
	pollEvery   = time.Second
	maxAttempts = 6
)

// Run sends due deliveries until ctx ends.
func (s *Sender) Run(ctx context.Context) {
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := s.SendDue(ctx); err != nil && ctx.Err() == nil {
			slog.Error("send notifications", "err", err)
		}
	}
}

// SendDue sends every delivery that is due now.
func (s *Sender) SendDue(ctx context.Context) error {
	rows, err := s.Pool.Query(ctx, `SELECT delivery_id, org_id FROM due_notifications()`)
	if err != nil {
		return err
	}
	type due struct{ id, org string }
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (due, error) {
		var d due
		return d, r.Scan(&d.id, &d.org)
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, d := range all {
		if err := s.send(ctx, d.id, d.org); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Sender) send(ctx context.Context, id, orgID string) error {
	return db.InOrg(ctx, s.Pool, orgID, func(tx pgx.Tx) error {
		var kind, event string
		var sealedURL, sealedSecret, payload []byte
		var attempts int
		err := tx.QueryRow(ctx, `SELECT n.kind, d.event, n.url, n.secret, d.payload, d.attempts FROM notification_deliveries d
			JOIN notification_destinations n ON n.id = d.destination_id
			WHERE d.id = $1 AND d.delivered_at IS NULL AND d.failed_at IS NULL FOR UPDATE OF d SKIP LOCKED`, id).
			Scan(&kind, &event, &sealedURL, &sealedSecret, &payload, &attempts)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := s.KEK.Open(ctx, tx, orgID, sealedURL)
		if err != nil {
			return err
		}
		var secret []byte
		if sealedSecret != nil {
			if secret, err = s.KEK.Open(ctx, tx, orgID, sealedSecret); err != nil {
				return err
			}
		}
		sendErr := post(ctx, s.Client, s.PublicURL, kind, string(target), id, event, secret, payload)
		attempts++
		switch {
		case sendErr == nil:
			_, err = tx.Exec(ctx, `UPDATE notification_deliveries SET delivered_at = now(), attempts = $2, last_error = NULL WHERE id = $1`, id, attempts)
		case attempts >= maxAttempts:
			_, err = tx.Exec(ctx, `UPDATE notification_deliveries SET failed_at = now(), attempts = $2, last_error = $3 WHERE id = $1`,
				id, attempts, sendErr.Error())
		default:
			// 20 s, 40 s, 80 s, …
			_, err = tx.Exec(ctx, `UPDATE notification_deliveries SET next_at = now() + make_interval(secs => 10 * 2 ^ $2), attempts = $2,
				last_error = $3 WHERE id = $1`, id, attempts, sendErr.Error())
		}
		return err
	})
}

// post sends one delivery. A Slack incoming webhook takes {"text": …};
// a webhook takes the event, signed.
// https://api.slack.com/messaging/webhooks
func post(ctx context.Context, client *http.Client, publicURL, kind, target, id, event string, secret, payload []byte) error {
	var e Event
	if err := json.Unmarshal(payload, &e); err != nil {
		return err
	}
	switch {
	case e.Deployment != "":
		e.URL = fmt.Sprintf("%s/%s/deployments/%s", publicURL, e.Org, e.Deployment)
	case e.Run > 0:
		e.URL = fmt.Sprintf("%s/%s/projects/%s/%s/runs/%d", publicURL, e.Org, e.Project, e.Workspace, e.Run)
	case e.Workspace != "":
		e.URL = fmt.Sprintf("%s/%s/projects/%s/%s/security", publicURL, e.Org, e.Project, e.Workspace)
	}
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if kind == "slack" {
		text := e.Text
		if e.URL != "" {
			text += " <" + e.URL + "|Open>"
		}
		if body, err = json.Marshal(map[string]string{"text": text}); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Stackwright")
	if kind == "webhook" {
		req.Header.Set("X-Stackwright-Event", event)
		req.Header.Set("X-Stackwright-Delivery", id)
		req.Header.Set("X-Stackwright-Signature", Sign(secret, body))
	}
	resp, err := client.Do(req)
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
