package server_test

import (
	"context"
	"encoding/base64"
	"testing"
	"time"
)

const bucketPrefix = `# METADATA
# title: Bucket names start with acme-
# custom:
#   id: ACME-0001
#   avd_id: ACME-0001
#   severity: HIGH
#   input:
#     selector:
#       - type: cloud
package user.acme.ACME0001

import rego.v1

deny contains res if {
	some bucket in input.aws.s3.buckets
	not startswith(bucket.name.value, "acme-")
	res := result.new("The bucket name does not start with acme-.", bucket.name)
}
`

func findingsOf(c *client, req map[string]any, procedure string) []map[string]any {
	var out []map[string]any
	for _, f := range asList(c.must(procedure, merge(map[string]any{"org": "a"}, req))["findings"]) {
		out = append(out, f.(map[string]any))
	}
	return out
}

func withCheck(fs []map[string]any, check string) []map[string]any {
	var out []map[string]any
	for _, f := range fs {
		if f["checkId"] == check {
			out = append(out, f)
		}
	}
	return out
}

func TestSecurityReview(t *testing.T) {
	t.Parallel()
	w := newRunWorld(t)
	w.project(w.owner, "a", "sec", "prod")
	w.project(w.owner, "a", "pol", "prod")
	security := w.member(w.owner, "a", "security@a.test", "ROLE_SECURITY")
	writer := w.member(w.owner, "a", "writer@a.test", "ROLE_WRITER")
	prod := map[string]any{"org": "a", "project": "sec", "workspace": "prod"}
	upload := func(fixture string, planOnly bool) map[string]any {
		return merge(prod, map[string]any{"source": base64.StdEncoding.EncodeToString(source(t, fixture)), "sourceName": fixture + ".tar.gz",
			"planOnly": planOnly})
	}
	run := func(n int) map[string]any { return merge(prod, map[string]any{"number": n}) }

	w.owner.must("RunService/CreateRun", upload("ssh-open", false))
	w.waitFor(prod, 1, "RUN_STATUS_BLOCKED")
	inRun := withCheck(findingsOf(w.viewer, run(1), "SecurityService/ListRunFindings"), "AWS-0107")
	if len(inRun) != 1 || inRun[0]["severity"] != "SEVERITY_HIGH" || inRun[0]["blocking"] != true || inRun[0]["resource"] != "aws_security_group.ssh" {
		t.Fatalf("run 1's findings: %v", inRun)
	}
	id := inRun[0]["id"].(string)

	if _, code := w.reviewer.call("RunService/ApproveRun", run(1)); code != "failed_precondition" {
		t.Errorf("approving a blocked run: %q", code)
	}
	in := func(days int) map[string]any {
		return map[string]any{"org": "a", "id": id, "reason": "a bastion, until the VPN is up",
			"expiresAt": time.Now().Add(time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)}
	}
	for name, c := range map[string]*client{"writer": writer, "reviewer": w.reviewer, "viewer": w.viewer} {
		if _, code := c.call("SecurityService/AcceptRisk", in(1)); code != "permission_denied" {
			t.Errorf("a %s accepted a risk: %q", name, code)
		}
	}
	if _, code := security.call("SecurityService/AcceptRisk", in(91)); code != "invalid_argument" {
		t.Errorf("accepting for 91 days: %q", code)
	}
	waiting := func(c *client) []any {
		return asList(c.must("RunService/ListWaiting", map[string]any{"org": "a"})["findings"])
	}
	if len(waiting(security)) != 1 || len(waiting(w.reviewer)) != 0 {
		t.Errorf("waiting findings: security %v, reviewer %v", waiting(security), waiting(w.reviewer))
	}

	security.must("SecurityService/AcceptRisk", in(1))
	w.waitFor(prod, 1, "RUN_STATUS_NEEDS_APPROVAL")
	if got := waiting(security); len(got) != 0 {
		t.Errorf("an accepted finding still waits: %v", got)
	}

	if _, err := w.world.owner.Exec(context.Background(), `UPDATE risk_acceptances SET accepted_at = now() - interval '2 days',
		expires_at = now() - interval '1 day' WHERE finding_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	w.reviewer.must("RunService/ApproveRun", run(1))
	w.waitFor(prod, 1, "RUN_STATUS_BLOCKED")
	w.reviewer.must("RunService/RejectRun", run(1))

	w.owner.must("RunService/CreateRun", upload("ssh-open", true))
	w.waitFor(prod, 2, "RUN_STATUS_PLANNED")
	if open := withCheck(findingsOf(w.viewer, prod, "SecurityService/ListFindings"), "AWS-0107"); len(open) != 1 || open[0]["id"] != id {
		t.Errorf("after two plans of the same code: %v", open)
	}

	w.owner.must("RunService/CreateRun", upload("ssh-closed", true))
	w.waitFor(prod, 3, "RUN_STATUS_PLANNED")
	if open := withCheck(findingsOf(w.viewer, prod, "SecurityService/ListFindings"), "AWS-0107"); len(open) != 0 {
		t.Errorf("open after the fix: %v", open)
	}
	all := withCheck(findingsOf(w.viewer, merge(prod, map[string]any{"includeFixed": true}), "SecurityService/ListFindings"), "AWS-0107")
	if len(all) != 1 || all[0]["fixed"] != true {
		t.Errorf("the fixed finding: %v", all)
	}

	pol := map[string]any{"org": "a", "project": "pol", "workspace": "prod"}
	if _, code := writer.call("SecurityService/SetPolicySet", map[string]any{"org": "a", "name": "naming", "rego": bucketPrefix}); code != "permission_denied" {
		t.Errorf("a writer set a policy: %q", code)
	}
	if _, code := security.call("SecurityService/SetPolicySet", map[string]any{"org": "a", "name": "naming", "rego": "package data.x\n"}); code != "invalid_argument" {
		t.Errorf("a policy outside user.: %q", code)
	}
	security.must("SecurityService/SetPolicySet", map[string]any{"org": "a", "name": "naming", "rego": bucketPrefix})
	security.must("SecurityService/SetSkippedChecks", map[string]any{"org": "a", "skippedChecks": []string{"AWS-0086"}})
	w.owner.must("RunService/CreateRun", merge(pol, map[string]any{"source": base64.StdEncoding.EncodeToString(source(t, "bucket")),
		"sourceName": "bucket.tar.gz", "planOnly": true}))
	w.waitFor(pol, 1, "RUN_STATUS_PLANNED")
	got := findingsOf(w.viewer, pol, "SecurityService/ListFindings")
	if len(withCheck(got, "ACME-0001")) != 1 || len(withCheck(got, "AWS-0086")) != 0 || len(withCheck(got, "AWS-0087")) != 1 {
		t.Errorf("the bucket's findings: %v", got)
	}

	slot := time.Now().Add(-time.Minute)
	if err := w.runs.QueueNightly(context.Background(), slot); err != nil {
		t.Fatal(err)
	}
	if err := w.runs.QueueNightly(context.Background(), slot); err != nil {
		t.Fatal(err)
	}
	for _, ws := range []map[string]any{prod, pol} {
		var scheduled []any
		for _, r := range asList(w.owner.must("RunService/ListRuns", ws)["runs"]) {
			if r := r.(map[string]any); r["trigger"] == "schedule" {
				if r["planOnly"] != true {
					t.Errorf("a scheduled run applies: %v", r)
				}
				scheduled = append(scheduled, r["number"])
			}
		}
		if len(scheduled) != 1 {
			t.Errorf("%v: scheduled runs %v, want one", ws["project"], scheduled)
		}
	}
	last := w.waitFor(prod, 4, "RUN_STATUS_PLANNED")
	if last["source"] != "ssh-closed.tar.gz" {
		t.Errorf("the nightly run planned %v, want the last upload", last["source"])
	}
	if open := withCheck(findingsOf(w.viewer, prod, "SecurityService/ListFindings"), "AWS-0107"); len(open) != 0 {
		t.Errorf("the nightly run of fixed code: %v", open)
	}
}

func TestModuleFindingsBlockByTheirAddress(t *testing.T) {
	t.Parallel()
	w := newRunWorld(t)
	w.project(w.owner, "a", "mod", "prod")
	prod := map[string]any{"org": "a", "project": "mod", "workspace": "prod"}
	w.owner.must("RunService/CreateRun", merge(prod, map[string]any{"source": base64.StdEncoding.EncodeToString(source(t, "ssh-module")),
		"sourceName": "ssh-module.tar.gz"}))
	w.waitFor(prod, 1, "RUN_STATUS_BLOCKED")
	stored := withCheck(findingsOf(w.viewer, prod, "SecurityService/ListFindings"), "AWS-0107")
	if len(stored) != 1 || stored[0]["resource"] != "module.bastion.aws_security_group.ssh" || stored[0]["unmatched"] == true {
		t.Fatalf("the module's finding: %v", stored)
	}
	inRun := withCheck(findingsOf(w.viewer, merge(prod, map[string]any{"number": 1}), "SecurityService/ListRunFindings"), "AWS-0107")
	if len(inRun) != 1 || inRun[0]["blocking"] != true || inRun[0]["resource"] != "module.bastion.aws_security_group.ssh" {
		t.Errorf("the run's finding: %v", inRun)
	}
}
