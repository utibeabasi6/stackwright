package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/internal/redact"
)

func TestUntarStaysInsideItsFolder(t *testing.T) {
	for _, name := range []string{"../escape.tf", "a/../../escape.tf"} {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte("x"))
		_ = tw.Close()
		_ = gz.Close()
		if err := untar(buf.Bytes(), t.TempDir()+"/src"); err == nil {
			t.Errorf("%s: unpacked outside the folder", name)
		}
	}
}

func TestParse(t *testing.T) {
	cases := []struct {
		line, typ, addr, absent string
	}{
		{`{"type":"planned_change","change":{"resource":{"addr":"terraform_data.a"},"action":"create"}}`, "engine.planned_change", "terraform_data.a", ""},
		{`{"type":"apply_start","hook":{"resource":{"addr":"terraform_data.b"}}}`, "engine.apply_start", "terraform_data.b", ""},
		{`{"type":"diagnostic","diagnostic":{"severity":"error","address":"terraform_data.c"}}`, "engine.diagnostic", "terraform_data.c", ""},
		{`{"type":"outputs","outputs":{"s":{"sensitive":true,"value":"hunter2"},"p":{"sensitive":false,"value":"ok"}}}`, "engine.outputs", "", "hunter2"},
		{`Initializing the backend...`, "engine.log", "", ""},
	}
	for _, c := range cases {
		e := parse([]byte(c.line))
		if e.typ != c.typ || e.resourceAddr != c.addr {
			t.Errorf("%s: got %s %q", c.line, e.typ, e.resourceAddr)
		}
		if c.absent != "" && bytes.Contains(e.payload, []byte(c.absent)) {
			t.Errorf("%s: payload keeps %q: %s", c.line, c.absent, e.payload)
		}
	}
	if e := parse([]byte(`{"type":"outputs","outputs":{"p":{"value":"ok"}}}`)); !bytes.Contains(e.payload, []byte(`"ok"`)) {
		t.Errorf("a plain output lost its value: %s", e.payload)
	}
}

func TestEngineEnvHasVariablesThenCredentials(t *testing.T) {
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("WebIdentityToken") != "web-identity-token" || r.Form.Get("RoleArn") != "arn:aws:iam::123456789012:role/plan" {
			http.Error(w, "<ErrorResponse><Error><Code>AccessDenied</Code></Error></ErrorResponse>", http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, `<AssumeRoleWithWebIdentityResponse><AssumeRoleWithWebIdentityResult><Credentials>
<AccessKeyId>ASIAEXAMPLE</AccessKeyId><SecretAccessKey>secret-access-key</SecretAccessKey><SessionToken>session-token</SessionToken>
<Expiration>2030-01-01T00:00:00Z</Expiration></Credentials></AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`)
	}))
	defer sts.Close()
	job := &v1.Job{
		Id: "j1",
		Variables: []*v1.JobVariable{
			{Category: v1.VariableCategory_VARIABLE_CATEGORY_TERRAFORM, Key: "region_name", Value: "eu"},
			{Category: v1.VariableCategory_VARIABLE_CATEGORY_ENV, Key: "DB_PASSWORD", Value: "hunter22", Sensitive: true},
		},
		Aws: &v1.AwsCredentials{RoleArn: "arn:aws:iam::123456789012:role/plan", Region: "eu-west-1", WebIdentityToken: "web-identity-token", StsEndpoint: sts.URL},
	}
	r := &runner{job: job, rep: &reporter{}, secrets: redact.New("hunter22", "web-identity-token")}
	env, err := r.engineEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"TF_VAR_region_name=eu", "DB_PASSWORD=hunter22", "AWS_ACCESS_KEY_ID=ASIAEXAMPLE",
		"AWS_SECRET_ACCESS_KEY=secret-access-key", "AWS_SESSION_TOKEN=session-token", "AWS_REGION=eu-west-1", "AWS_DEFAULT_REGION=eu-west-1"}
	if !slices.Equal(env, want) {
		t.Errorf("env = %q", env)
	}
	line := r.redact([]byte("key secret-access-key token session-token pw hunter22 jwt web-identity-token id ASIAEXAMPLE"))
	if string(line) != "key (sensitive value) token (sensitive value) pw (sensitive value) jwt (sensitive value) id ASIAEXAMPLE" {
		t.Errorf("redacted: %s", line)
	}
	if len(r.rep.pending) != 1 || r.rep.pending[0].Type != "executor.credentials_issued" || bytes.Contains(r.rep.pending[0].Payload, []byte("secret-access-key")) {
		t.Errorf("events: %v", r.rep.pending)
	}

	job.Aws.RoleArn = "arn:aws:iam::123456789012:role/apply"
	if _, err := r.engineEnv(context.Background()); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("a role STS refuses: %v", err)
	}
}
