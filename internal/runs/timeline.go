package runs

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	v1 "github.com/utibeabasi6/stackwright/gen/stackwright/v1"
)

var engineNames = map[string]string{"ENGINE_OPENTOFU": "OpenTofu", "ENGINE_TERRAFORM": "Terraform"}

// Describe writes an event's line of the timeline, which the web app and
// the CLI both print (adr/0044).
func Describe(e *v1.RunEvent) {
	var p map[string]any
	_ = json.Unmarshal([]byte(e.GetPayload()), &p)
	str := func(k string) string {
		if v, ok := p[k]; ok && v != nil {
			return fmt.Sprint(v)
		}
		return ""
	}
	truthy := func(k string) bool { b, _ := p[k].(bool); return b }
	by := ""
	if e.GetActor() != "" {
		by = " by " + e.GetActor()
	}
	e.Text, e.Tone, e.Detail, e.Fix = "", "", "", ""
	switch e.GetType() {
	case "run.queued":
		e.Text = fmt.Sprintf("Queued%s, from %s", by, str("source"))
		if truthy("plan_only") {
			e.Text += ", plan only"
		}
	case "run.planning":
		e.Text = "The workspace is free; waiting for a build server"
	case "run.waiting":
		labels, _ := p["labels"].(map[string]any)
		var kv []string
		for k, v := range labels {
			kv = append(kv, fmt.Sprintf("%s=%v", k, v))
		}
		slices.Sort(kv)
		e.Text, e.Tone = "Waits for a build server with a free slot", "warn"
		if len(kv) > 0 {
			e.Text = fmt.Sprintf("Waits for a build server with %s and a free slot", strings.Join(kv, ", "))
		}
	case "run.dispatched":
		e.Text = fmt.Sprintf("Sent the %s to build server %s", str("operation"), str("build_server"))
	case "run.summary":
		e.Text = "Plan: no changes"
		if truthy("has_changes") {
			e.Text = fmt.Sprintf("Plan: +%s ~%s −%s", str("adds"), str("changes"), str("destroys"))
		}
	case "run.needs_approval":
		e.Text, e.Tone = "Waits for approval", "warn"
	case "executor.units":
		units, _ := p["units"].([]any)
		names := make([]string, len(units))
		for i, u := range units {
			names[i] = fmt.Sprint(u)
		}
		e.Text = "Terragrunt units, in order: " + strings.Join(names, ", ")
	case "executor.unit_started":
		e.Text = "Unit " + str("unit")
	case "run.blocked":
		e.Text, e.Tone = "! A High or Critical finding blocks the apply", "error"
	case "run.unblocked":
		e.Text = "An accepted risk unblocked the apply"
	case "run.auto_approved":
		e.Text = "Auto-apply is on, so the apply starts"
	case "run.approved":
		e.Text = "Approved" + by
	case "run.planned":
		e.Text, e.Tone = "✓ Planned", "ok"
	case "run.applied":
		e.Text, e.Tone = "✓ Applied", "ok"
	case "run.failed":
		e.Text, e.Tone = "! Failed: "+str("error"), "error"
	case "run.rejected":
		e.Text = "Rejected" + by
	case "run.canceled":
		e.Text = "Canceled" + by
	case "executor.started":
		engine := engineNames[str("engine")]
		if engine == "" {
			engine = str("engine")
		}
		e.Text = fmt.Sprintf("Sandbox started: %s %s", engine, str("version"))
	case "executor.engine_ready":
		e.Text = "The engine binary matches its signed checksum"
		if truthy("cached") {
			e.Text = "The engine binary from the build server's cache matches its signed checksum"
		}
	case "executor.credentials_issued":
		until := str("expires_at")
		if t, err := time.Parse(time.RFC3339, until); err == nil {
			until = t.UTC().Format("15:04:05") + " UTC"
		}
		e.Text = fmt.Sprintf("AWS credentials issued for %s, until %s", str("role_arn"), until)
	case "executor.failed":
		e.Text, e.Tone = fmt.Sprintf("! %s failed: %s", str("stage"), str("error")), "error"
	case "engine.diagnostic":
		// https://opentofu.org/docs/internals/machine-readable-ui/#diagnostic
		var d struct {
			Severity string `json:"severity"`
			Summary  string `json:"summary"`
			Detail   string `json:"detail"`
			Range    *struct {
				Filename string `json:"filename"`
				Start    struct {
					Line int `json:"line"`
				} `json:"start"`
			} `json:"range"`
		}
		if raw, err := json.Marshal(p["diagnostic"]); err == nil {
			_ = json.Unmarshal(raw, &d)
		}
		where := ""
		if d.Range != nil && d.Range.Filename != "" {
			where = " (" + d.Range.Filename
			if d.Range.Start.Line > 0 {
				where += fmt.Sprintf(":%d", d.Range.Start.Line)
			}
			where += ")"
		}
		summary := d.Summary
		if summary == "" {
			summary = "Diagnostic"
		}
		e.Detail, e.Tone = d.Detail, "warn"
		e.Text = summary + where
		if d.Severity == "error" {
			e.Text, e.Tone = "! "+e.Text, "error"
			if d.Summary == "" {
				summary = "Error"
			}
			e.Fix = summary + where
			if d.Detail != "" {
				e.Fix += "\n" + d.Detail
			}
		}
	case "executor.finished":
		e.Text = "Sandbox finished"
		if !truthy("succeeded") {
			e.Text, e.Tone = "Sandbox finished with an error", "error"
		}
	default:
		e.Text = e.GetType()
		if msg, ok := p["@message"].(string); ok {
			e.Text = msg
		}
		if p["@level"] == "error" {
			e.Tone = "error"
		}
	}
	e.Text = strings.TrimSpace(e.Text)
}
