package api

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/gen/stackwright/v1/stackwrightv1connect"
	"github.com/stackwright/stackwright/internal/auth"
	"github.com/stackwright/stackwright/internal/authz"
	"github.com/stackwright/stackwright/internal/notify"
	"github.com/stackwright/stackwright/internal/seal"
)

// Notifications sets up an org's notification destinations and audit
// export (adr/0045).
type Notifications struct {
	stackwrightv1connect.UnimplementedNotificationServiceHandler
	KEK       seal.KEK
	AllowHTTP bool
}

var eventNames = map[v1.NotificationEvent]string{
	v1.NotificationEvent_NOTIFICATION_EVENT_RUN_FAILED:         notify.RunFailed,
	v1.NotificationEvent_NOTIFICATION_EVENT_RUN_NEEDS_APPROVAL: notify.RunNeedsApproval,
	v1.NotificationEvent_NOTIFICATION_EVENT_RUN_APPLIED:        notify.RunApplied,
	v1.NotificationEvent_NOTIFICATION_EVENT_FINDING_OPENED:     notify.FindingOpened,
	v1.NotificationEvent_NOTIFICATION_EVENT_DRIFT_FOUND:        notify.DriftFound,
}

func events(in []v1.NotificationEvent) ([]string, error) {
	var out []string
	for _, e := range in {
		name, ok := eventNames[e]
		if !ok {
			return nil, invalid("choose events from the list")
		}
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return nil, invalid("choose at least one event")
	}
	return out, nil
}

func (h Notifications) ListDestinations(ctx context.Context, _ *connect.Request[v1.ListDestinationsRequest]) (*connect.Response[v1.ListDestinationsResponse], error) {
	s := authz.ScopeFrom(ctx)
	rows, err := s.Tx.Query(ctx, `SELECT n.id, n.kind, n.name, n.url, n.events,
		(SELECT max(delivered_at) FROM notification_deliveries d WHERE d.destination_id = n.id),
		(SELECT last_error FROM notification_deliveries d WHERE d.destination_id = n.id AND d.last_error IS NOT NULL ORDER BY d.created_at DESC LIMIT 1)
		FROM notification_destinations n ORDER BY n.name`)
	if err != nil {
		return nil, authz.Internal(err)
	}
	type row struct {
		d      *v1.Destination
		sealed []byte
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		x := row{d: &v1.Destination{}}
		var kind string
		var names []string
		var delivered *time.Time
		var lastErr *string
		err := r.Scan(&x.d.Id, &kind, &x.d.Name, &x.sealed, &names, &delivered, &lastErr)
		x.d.Kind = v1.DestinationKind_DESTINATION_KIND_WEBHOOK
		if kind == "slack" {
			x.d.Kind = v1.DestinationKind_DESTINATION_KIND_SLACK
		}
		for e, n := range eventNames {
			if slices.Contains(names, n) {
				x.d.Events = append(x.d.Events, e)
			}
		}
		slices.Sort(x.d.Events)
		if delivered != nil {
			x.d.LastDeliveredAt = timestamppb.New(*delivered)
		}
		if lastErr != nil {
			x.d.LastError = *lastErr
		}
		return x, err
	})
	if err != nil {
		return nil, authz.Internal(err)
	}
	out := &v1.ListDestinationsResponse{}
	for _, x := range all {
		plain, err := h.KEK.Open(ctx, s.Tx, s.OrgID, x.sealed)
		if err != nil {
			return nil, authz.Internal(err)
		}
		if u, err := checkURL(string(plain), true); err == nil {
			x.d.Host = u.Host
		}
		out.Destinations = append(out.Destinations, x.d)
	}
	return connect.NewResponse(out), nil
}

func (h Notifications) CreateDestination(ctx context.Context, req *connect.Request[v1.CreateDestinationRequest]) (*connect.Response[v1.CreateDestinationResponse], error) {
	s := authz.ScopeFrom(ctx)
	m := req.Msg
	name := strings.TrimSpace(m.Name)
	if name == "" || len(name) > 100 {
		return nil, invalid("name the destination in 1 to 100 characters")
	}
	u, err := checkURL(m.Url, h.AllowHTTP)
	if err != nil {
		return nil, err
	}
	names, err := events(m.Events)
	if err != nil {
		return nil, err
	}
	kind := "webhook"
	switch m.Kind {
	case v1.DestinationKind_DESTINATION_KIND_SLACK:
		kind = "slack"
	case v1.DestinationKind_DESTINATION_KIND_WEBHOOK:
	default:
		return nil, invalid("choose Slack or a webhook")
	}
	sealedURL, err := h.KEK.Seal(ctx, s.Tx, s.OrgID, []byte(u.String()))
	if err != nil {
		return nil, authz.Internal(err)
	}
	secret := ""
	var sealedSecret []byte
	if kind == "webhook" {
		secret = auth.RandomToken()
		if sealedSecret, err = h.KEK.Seal(ctx, s.Tx, s.OrgID, []byte(secret)); err != nil {
			return nil, authz.Internal(err)
		}
	}
	id := uuid.NewString()
	_, err = s.Tx.Exec(ctx, `INSERT INTO notification_destinations (id, org_id, kind, name, url, secret, events) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id, s.OrgID, kind, name, sealedURL, sealedSecret, names)
	if uniqueViolation(err) {
		return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("a destination has that name"))
	}
	if err != nil {
		return nil, authz.Internal(err)
	}
	s.Target("notification destination " + name)
	d := &v1.Destination{Id: id, Kind: m.Kind, Name: name, Host: u.Host, Events: m.Events}
	return connect.NewResponse(&v1.CreateDestinationResponse{Destination: d, Secret: secret}), nil
}

func (h Notifications) UpdateDestinationEvents(ctx context.Context, req *connect.Request[v1.UpdateDestinationEventsRequest]) (*connect.Response[v1.UpdateDestinationEventsResponse], error) {
	s := authz.ScopeFrom(ctx)
	names, err := events(req.Msg.Events)
	if err != nil {
		return nil, err
	}
	if err := h.change(ctx, s, `UPDATE notification_destinations SET events = $2 WHERE id = $1 RETURNING name`, req.Msg.Id, names); err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.UpdateDestinationEventsResponse{}), nil
}

func (h Notifications) DeleteDestination(ctx context.Context, req *connect.Request[v1.DeleteDestinationRequest]) (*connect.Response[v1.DeleteDestinationResponse], error) {
	s := authz.ScopeFrom(ctx)
	if err := h.change(ctx, s, `DELETE FROM notification_destinations WHERE id = $1 RETURNING name`, req.Msg.Id); err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.DeleteDestinationResponse{}), nil
}

func (h Notifications) TestDestination(ctx context.Context, req *connect.Request[v1.TestDestinationRequest]) (*connect.Response[v1.TestDestinationResponse], error) {
	s := authz.ScopeFrom(ctx)
	if err := h.change(ctx, s, `SELECT name FROM notification_destinations WHERE id = $1`, req.Msg.Id); err != nil {
		return nil, err
	}
	if err := notify.EnqueueTest(ctx, s.Tx, s.OrgID, req.Msg.Org, req.Msg.Id); err != nil {
		return nil, authz.Internal(err)
	}
	return connect.NewResponse(&v1.TestDestinationResponse{}), nil
}

// change runs q on one destination, which returns its name for the audit
// row.
func (h Notifications) change(ctx context.Context, s *authz.Scope, q, id string, args ...any) error {
	if _, err := uuid.Parse(id); err != nil {
		return errNotFound
	}
	var name string
	err := s.Tx.QueryRow(ctx, q, append([]any{id}, args...)...).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return authz.Internal(err)
	}
	s.Target("notification destination " + name)
	return nil
}

func (h Notifications) loadExport(ctx context.Context, tx pgx.Tx, orgID string) (*v1.GetAuditExportResponse, error) {
	out := &v1.GetAuditExportResponse{}
	if err := tx.QueryRow(ctx, `SELECT audit_retention_days FROM orgs WHERE id = $1`, orgID).Scan(&out.RetentionDays); err != nil {
		return nil, authz.Internal(err)
	}
	rows, err := tx.Query(ctx, `SELECT e.kind, e.bucket, e.prefix, coalesce(a.name, ''), e.url, e.sent_id, e.sent_at, coalesce(e.last_error, '')
		FROM audit_exports e LEFT JOIN cloud_accounts a ON a.id = e.cloud_account_id`)
	if err != nil {
		return nil, authz.Internal(err)
	}
	type row struct {
		kind, bucket, prefix, account string
		sealedURL                     []byte
		target                        *v1.AuditExportTarget
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		x := row{target: &v1.AuditExportTarget{}}
		var sent *time.Time
		err := r.Scan(&x.kind, &x.bucket, &x.prefix, &x.account, &x.sealedURL, &x.target.SentId, &sent, &x.target.LastError)
		if sent != nil {
			x.target.SentAt = timestamppb.New(*sent)
		}
		return x, err
	})
	if err != nil {
		return nil, authz.Internal(err)
	}
	for _, x := range all {
		if x.kind == "s3" {
			out.S3Bucket, out.S3Prefix, out.S3CloudAccount, out.S3 = x.bucket, x.prefix, x.account, x.target
			continue
		}
		plain, err := h.KEK.Open(ctx, tx, orgID, x.sealedURL)
		if err != nil {
			return nil, authz.Internal(err)
		}
		if u, err := checkURL(string(plain), true); err == nil {
			out.WebhookHost = u.Host
		}
		out.Webhook = x.target
	}
	return out, nil
}

func (h Notifications) GetAuditExport(ctx context.Context, _ *connect.Request[v1.GetAuditExportRequest]) (*connect.Response[v1.GetAuditExportResponse], error) {
	s := authz.ScopeFrom(ctx)
	out, err := h.loadExport(ctx, s.Tx, s.OrgID)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

func (h Notifications) SaveAuditExport(ctx context.Context, req *connect.Request[v1.SaveAuditExportRequest]) (*connect.Response[v1.SaveAuditExportResponse], error) {
	s := authz.ScopeFrom(ctx)
	m := req.Msg
	if m.RetentionDays < 30 || m.RetentionDays > 3650 {
		return nil, invalid("keep the audit log for 30 to 3650 days")
	}
	if _, err := s.Tx.Exec(ctx, `UPDATE orgs SET audit_retention_days = $2 WHERE id = $1`, s.OrgID, m.RetentionDays); err != nil {
		return nil, authz.Internal(err)
	}
	if bucket := strings.TrimSpace(m.S3Bucket); bucket == "" {
		if _, err := s.Tx.Exec(ctx, `DELETE FROM audit_exports WHERE kind = 's3'`); err != nil {
			return nil, authz.Internal(err)
		}
	} else {
		account, err := oneAccount(ctx, s.Tx, m.S3CloudAccount)
		if err != nil {
			return nil, err
		}
		prefix := strings.Trim(strings.TrimSpace(m.S3Prefix), "/")
		if prefix != "" {
			prefix += "/"
		}
		if _, err := s.Tx.Exec(ctx, `INSERT INTO audit_exports (org_id, kind, bucket, prefix, cloud_account_id) VALUES ($1, 's3', $2, $3, $4)
			ON CONFLICT (org_id, kind) DO UPDATE SET bucket = $2, prefix = $3, cloud_account_id = $4`, s.OrgID, bucket, prefix, account.GetId()); err != nil {
			return nil, authz.Internal(err)
		}
	}
	secret := ""
	switch {
	case m.StopWebhook:
		if _, err := s.Tx.Exec(ctx, `DELETE FROM audit_exports WHERE kind = 'webhook'`); err != nil {
			return nil, authz.Internal(err)
		}
	case m.WebhookUrl != "":
		u, err := checkURL(m.WebhookUrl, h.AllowHTTP)
		if err != nil {
			return nil, err
		}
		sealedURL, err := h.KEK.Seal(ctx, s.Tx, s.OrgID, []byte(u.String()))
		if err != nil {
			return nil, authz.Internal(err)
		}
		secret = auth.RandomToken()
		sealedSecret, err := h.KEK.Seal(ctx, s.Tx, s.OrgID, []byte(secret))
		if err != nil {
			return nil, authz.Internal(err)
		}
		if _, err := s.Tx.Exec(ctx, `INSERT INTO audit_exports (org_id, kind, url, secret) VALUES ($1, 'webhook', $2, $3)
			ON CONFLICT (org_id, kind) DO UPDATE SET url = $2, secret = $3`, s.OrgID, sealedURL, sealedSecret); err != nil {
			return nil, authz.Internal(err)
		}
	}
	s.Target("audit export")
	out, err := h.loadExport(ctx, s.Tx, s.OrgID)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.SaveAuditExportResponse{Export: out, WebhookSecret: secret}), nil
}
