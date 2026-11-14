package runs

import (
	"testing"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
)

func TestDescribeWritesTheTimelinesLine(t *testing.T) {
	for _, tc := range []struct {
		typ, payload, actor     string
		text, tone, detail, fix string
	}{
		{typ: "run.queued", payload: `{"source": "cli on laptop:infra", "plan_only": true}`, actor: "Kim",
			text: "Queued by Kim, from cli on laptop:infra, plan only"},
		{typ: "run.waiting", payload: `{"labels": {"zone": "b", "arch": "arm64"}}`,
			text: "Waits for a build server with arch=arm64, zone=b and a free slot", tone: "warn"},
		{typ: "run.summary", payload: `{"has_changes": true, "adds": 2, "changes": 0, "destroys": 1}`, text: "Plan: +2 ~0 −1"},
		{typ: "executor.credentials_issued", payload: `{"role_arn": "arn:aws:iam::1:role/plan", "expires_at": "2026-11-14T10:05:00+01:00"}`,
			text: "AWS credentials issued for arn:aws:iam::1:role/plan, until 09:05:00 UTC"},
		{typ: "engine.diagnostic", payload: `{"diagnostic": {"severity": "error", "summary": "Unsupported argument", "detail": "nope is not expected",
			"range": {"filename": "main.tf", "start": {"line": 3}}}}`,
			text: "! Unsupported argument (main.tf:3)", tone: "error", detail: "nope is not expected",
			fix: "Unsupported argument (main.tf:3)\nnope is not expected"},
		{typ: "engine.diagnostic", payload: `{"diagnostic": {"severity": "warning", "summary": "Deprecated"}}`, text: "Deprecated", tone: "warn"},
		{typ: "engine.log", payload: `{"@message": "\nInitializing the backend...\n", "@level": "info"}`, text: "Initializing the backend..."},
		{typ: "engine.log", payload: `not json`, text: "engine.log"},
	} {
		e := &v1.RunEvent{Type: tc.typ, Payload: tc.payload, Actor: tc.actor}
		Describe(e)
		if e.Text != tc.text || e.Tone != tc.tone || e.Detail != tc.detail || e.Fix != tc.fix {
			t.Errorf("%s %s:\ngot  %q %q %q %q\nwant %q %q %q %q", tc.typ, tc.payload, e.Text, e.Tone, e.Detail, e.Fix, tc.text, tc.tone, tc.detail, tc.fix)
		}
	}
}
