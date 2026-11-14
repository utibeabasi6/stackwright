package server_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stackwright/stackwright/internal/notify"
	"github.com/stackwright/stackwright/internal/oidc"
)

// hook records the requests a fake receiver gets.
type hook struct {
	mu     sync.Mutex
	srv    *httptest.Server
	got    []hookPost
	status int
}

type hookPost struct {
	at     time.Time
	path   string
	header http.Header
	body   []byte
}

func newHook(t *testing.T) *hook {
	h := &hook{status: http.StatusOK}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		defer h.mu.Unlock()
		h.got = append(h.got, hookPost{at: time.Now(), path: r.URL.Path, header: r.Header.Clone(), body: b})
		w.WriteHeader(h.status)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hook) all() []hookPost {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hookPost(nil), h.got...)
}

func TestAFailedApplyPostsToSlackWithinTenSeconds(t *testing.T) {
	t.Parallel()
	w := newRunWorld(t)
	slack, webhook := newHook(t), newHook(t)
	go (&notify.Sender{Pool: w.runs.Pool, KEK: w.runs.KEK, Client: http.DefaultClient, PublicURL: w.srv.URL}).Run(t.Context())
	org := map[string]any{"org": "a"}
	if _, code := w.owner.call("NotificationService/CreateDestination", merge(org, map[string]any{"kind": "DESTINATION_KIND_SLACK",
		"name": "ops", "url": "ftp://example.test/x", "events": []string{"NOTIFICATION_EVENT_RUN_FAILED"}})); code != "invalid_argument" {
		t.Errorf("an ftp URL: %q", code)
	}
	if _, code := w.reviewer.call("NotificationService/ListDestinations", org); code != "permission_denied" {
		t.Errorf("a reviewer listed destinations: %q", code)
	}
	w.owner.must("NotificationService/CreateDestination", merge(org, map[string]any{"kind": "DESTINATION_KIND_SLACK", "name": "ops",
		"url": slack.srv.URL + "/services/T0/B0/x", "events": []string{"NOTIFICATION_EVENT_RUN_FAILED"}}))
	created := w.owner.must("NotificationService/CreateDestination", merge(org, map[string]any{"kind": "DESTINATION_KIND_WEBHOOK",
		"name": "siem", "url": webhook.srv.URL + "/hook", "events": []string{"NOTIFICATION_EVENT_RUN_FAILED", "NOTIFICATION_EVENT_RUN_NEEDS_APPROVAL"}}))
	secret := created["secret"].(string)

	w.project(w.owner, "a", "boom", "prod")
	ws := map[string]any{"org": "a", "project": "boom", "workspace": "prod"}
	w.owner.must("RunService/CreateRun", merge(ws, map[string]any{"source": base64.StdEncoding.EncodeToString(source(t, "fails-on-apply")),
		"sourceName": "boom.tar.gz"}))
	w.waitFor(ws, 1, "RUN_STATUS_NEEDS_APPROVAL")
	w.reviewer.must("RunService/ApproveRun", merge(ws, map[string]any{"number": 1}))
	w.waitFor(ws, 1, "RUN_STATUS_FAILED")
	var failedAt time.Time
	if err := w.world.owner.QueryRow(t.Context(), `SELECT at FROM run_events WHERE type = 'run.failed'`).Scan(&failedAt); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a Slack post", func() bool { return len(slack.all()) > 0 })
	post := slack.all()[0]
	if took := post.at.Sub(failedAt); took > 10*time.Second {
		t.Errorf("Slack got the failure %s after it happened", took)
	}
	var msg struct{ Text string }
	if err := json.Unmarshal(post.body, &msg); err != nil || !strings.HasPrefix(msg.Text, "✗ Run #1 of boom/prod failed: ") ||
		!strings.Contains(msg.Text, "<"+w.srv.URL+"/a/projects/boom/prod/runs/1|Open>") {
		t.Errorf("the Slack post: %s", post.body)
	}

	eventually(t, "two webhook posts", func() bool { return len(webhook.all()) == 2 })
	var events []string
	for _, r := range webhook.all() {
		events = append(events, r.header.Get("X-Stackwright-Event"))
		if r.header.Get("X-Stackwright-Signature") != notify.Sign([]byte(secret), r.body) {
			t.Errorf("a webhook post is not signed with the secret: %s", r.header)
		}
	}
	if strings.Join(events, ",") != "run_needs_approval,run_failed" {
		t.Errorf("webhook events: %v", events)
	}

	// A test reaches only the destination it names.
	id := created["destination"].(map[string]any)["id"].(string)
	w.owner.must("NotificationService/TestDestination", merge(org, map[string]any{"id": id}))
	eventually(t, "a test post", func() bool { return len(webhook.all()) == 3 })
	if len(slack.all()) != 1 {
		t.Errorf("Slack got %d posts", len(slack.all()))
	}
	list := asList(w.owner.must("NotificationService/ListDestinations", org)["destinations"])
	if len(list) != 2 || list[1].(map[string]any)["host"] != strings.TrimPrefix(webhook.srv.URL, "http://") {
		t.Errorf("destinations: %v", list)
	}
}

func TestAuditExportSendsEveryRowAndKeepsWhatAnExportLacks(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	owner := w.setup()
	s3, webhook := newHook(t), newHook(t)
	org := map[string]any{"org": "a"}
	account := owner.must("CloudAccountService/CreateCloudAccount", merge(org, map[string]any{"name": "logs", "aws": map[string]any{
		"planRoleArn": "arn:aws:iam::123456789012:role/plan", "applyRoleArn": "arn:aws:iam::123456789012:role/audit", "region": "eu-west-1"}}))
	_ = account
	var orgID string
	if err := w.owner.QueryRow(t.Context(), `SELECT id FROM orgs WHERE slug = 'a'`).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	w.sts.mu.Lock()
	w.sts.trust["arn:aws:iam::123456789012:role/audit"] = []string{oidc.Subject(orgID, "", notify.AuditPhase)}
	w.sts.mu.Unlock()
	saved := owner.must("NotificationService/SaveAuditExport", merge(org, map[string]any{"retentionDays": 365, "s3Bucket": "audit-logs",
		"s3Prefix": "stackwright", "s3CloudAccount": "logs", "webhookUrl": webhook.srv.URL + "/audit"}))
	secret := saved["webhookSecret"].(string)
	stsURL := "http://127.0.0.1:" + w.sts.url[strings.LastIndex(w.sts.url, ":")+1:]
	auditor := &notify.Auditor{Pool: w.runs.Pool, KEK: w.runs.KEK, Issuer: w.jobs.Issuer, STSEndpoint: stsURL, S3Endpoint: s3.srv.URL,
		Client: http.DefaultClient}

	var last int64
	if err := w.owner.QueryRow(t.Context(), `SELECT max(id) FROM audit_log WHERE org_id = $1`, orgID).Scan(&last); err != nil {
		t.Fatal(err)
	}
	if err := auditor.ExportDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	puts, posts := s3.all(), webhook.all()
	if len(puts) != 1 || !strings.HasPrefix(puts[0].path, "/audit-logs/stackwright/"+time.Now().UTC().Format("2006/01/02")+"/") ||
		!strings.HasSuffix(puts[0].path, "-"+itoa(last)+".jsonl") {
		t.Fatalf("S3 got %v", puts)
	}
	// Over plain HTTP, minio signs the object in chunks, so the S3 body
	// holds the rows inside chunk headers.
	if len(posts) != 1 || !bytes.Contains(puts[0].body, posts[0].body) ||
		posts[0].header.Get("X-Stackwright-Signature") != notify.Sign([]byte(secret), posts[0].body) {
		t.Fatalf("the webhook got %d posts: %v", len(posts), posts)
	}
	if !strings.Contains(string(puts[0].body), `"action":"NotificationService/SaveAuditExport"`) {
		t.Errorf("the export lacks the export's own row:\n%s", puts[0].body)
	}
	if issued := w.sts.all(); len(issued) != 1 || issued[0].sub != oidc.Subject(orgID, "", notify.AuditPhase) {
		t.Errorf("STS issued %v", issued)
	}
	got := owner.must("NotificationService/GetAuditExport", org)
	if got["s3"].(map[string]any)["sentId"] != itoa(last) || got["webhook"].(map[string]any)["sentId"] != itoa(last) {
		t.Errorf("the cursors: %v", got)
	}

	// Rows past the retention go once every export has them.
	count := func() int {
		var n int
		if err := w.owner.QueryRow(t.Context(), `SELECT count(*) FROM audit_log WHERE org_id = $1`, orgID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := w.owner.Exec(t.Context(), `UPDATE audit_log SET at = now() - interval '400 days' WHERE org_id = $1`, orgID); err != nil {
		t.Fatal(err)
	}
	webhook.mu.Lock()
	webhook.status = http.StatusInternalServerError
	webhook.mu.Unlock()
	owner.must("NotificationService/SaveAuditExport", merge(org, map[string]any{"retentionDays": 365, "s3Bucket": "audit-logs",
		"s3Prefix": "stackwright", "s3CloudAccount": "logs", "webhookUrl": webhook.srv.URL + "/audit"}))
	if _, err := w.owner.Exec(t.Context(), `UPDATE audit_log SET at = now() - interval '400 days' WHERE org_id = $1`, orgID); err != nil {
		t.Fatal(err)
	}
	if err := auditor.ExportDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Errorf("%d rows left; want only the one the failing webhook lacks", n)
	}
	if e := owner.must("NotificationService/GetAuditExport", org)["webhook"].(map[string]any); !strings.Contains(e["lastError"].(string), "500") {
		t.Errorf("the webhook's error: %v", e)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return strings.Trim(string(b), `"`)
}
