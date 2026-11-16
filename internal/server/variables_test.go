package server_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stackwright/stackwright/internal/oidc"
)

// fakeSTS answers AssumeRoleWithWebIdentity as AWS does: it fetches the
// issuer's keys from the server, checks the token's signature, issuer,
// audience and expiry, and then the role's trust policy, which allows a
// list of subjects.
type fakeSTS struct {
	t   *testing.T
	srv *httptest.Server
	url string

	mu     sync.Mutex
	trust  map[string][]string
	issued []issued
}

type issued struct {
	role, sub, token string
}

const (
	stsSecretKey    = "fake-secret-access-key-0123"
	stsSessionToken = "fake-session-token-4567"
)

func newFakeSTS(t *testing.T, server *httptest.Server) *fakeSTS {
	f := &fakeSTS{t: t, srv: server, trust: map[string][]string{}}
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &httptest.Server{Listener: l, Config: &http.Server{Handler: f, ReadHeaderTimeout: 5 * time.Second}}
	s.Start()
	t.Cleanup(s.Close)
	f.url = fmt.Sprintf("http://host.docker.internal:%d", l.Addr().(*net.TCPAddr).Port)
	return f
}

// allow sets a role's trust policy, as GetAwsSetup wrote it.
func (f *fakeSTS) allow(role, policy string) {
	var p struct {
		Statement []struct {
			Condition struct{ StringEquals map[string]any }
		}
	}
	if err := json.Unmarshal([]byte(policy), &p); err != nil {
		f.t.Fatal(err)
	}
	var subs []string
	for k, v := range p.Statement[0].Condition.StringEquals {
		if strings.HasSuffix(k, ":sub") {
			for _, s := range v.([]any) {
				subs = append(subs, s.(string))
			}
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.trust[role] = subs
}

func (f *fakeSTS) all() []issued {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.issued)
}

func (f *fakeSTS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	deny := func(msg string) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprintf(w, "<ErrorResponse><Error><Code>AccessDenied</Code><Message>%s</Message></Error></ErrorResponse>", msg)
	}
	if err := r.ParseForm(); err != nil || r.Form.Get("Action") != "AssumeRoleWithWebIdentity" {
		deny("not AssumeRoleWithWebIdentity")
		return
	}
	token, role := r.Form.Get("WebIdentityToken"), r.Form.Get("RoleArn")
	claims, err := f.verify(token)
	if err != nil {
		deny(err.Error())
		return
	}
	f.mu.Lock()
	allowed := slices.Contains(f.trust[role], claims.Sub)
	if allowed {
		f.issued = append(f.issued, issued{role: role, sub: claims.Sub, token: token})
	}
	f.mu.Unlock()
	if !allowed {
		deny("Not authorized to perform sts:AssumeRoleWithWebIdentity")
		return
	}
	_, _ = fmt.Fprintf(w, `<AssumeRoleWithWebIdentityResponse><AssumeRoleWithWebIdentityResult><Credentials>
<AccessKeyId>ASIAFAKE</AccessKeyId><SecretAccessKey>%s</SecretAccessKey><SessionToken>%s</SessionToken>
<Expiration>%s</Expiration></Credentials></AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`,
		stsSecretKey, stsSessionToken, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
}

type tokenClaims struct {
	Iss, Sub, Aud string
	Exp           int64
}

func (f *fakeSTS) verify(token string) (tokenClaims, error) {
	var c tokenClaims
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return c, fmt.Errorf("not a JWT")
	}
	var header struct{ Alg, Kid string }
	h, _ := base64.RawURLEncoding.DecodeString(parts[0])
	p, _ := base64.RawURLEncoding.DecodeString(parts[1])
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	if json.Unmarshal(h, &header) != nil || json.Unmarshal(p, &c) != nil || header.Alg != "RS256" {
		return c, fmt.Errorf("malformed token")
	}
	resp, err := http.Get(f.srv.URL + "/.well-known/jwks.json")
	if err != nil {
		return c, err
	}
	defer func() { _ = resp.Body.Close() }()
	var jwks struct{ Keys []struct{ Kid, N, E string } }
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return c, err
	}
	for _, k := range jwks.Keys {
		if k.Kid != header.Kid {
			continue
		}
		n, _ := base64.RawURLEncoding.DecodeString(k.N)
		digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if err := rsa.VerifyPKCS1v15(&rsa.PublicKey{N: new(big.Int).SetBytes(n), E: 65537}, crypto.SHA256, digest[:], sig); err != nil {
			return c, fmt.Errorf("bad signature")
		}
		switch {
		case c.Iss != testIssuer:
			return c, fmt.Errorf("issuer %s", c.Iss)
		case c.Aud != oidc.AWSAudience:
			return c, fmt.Errorf("audience %s", c.Aud)
		case time.Unix(c.Exp, 0).Before(time.Now()):
			return c, fmt.Errorf("expired")
		}
		return c, nil
	}
	return c, fmt.Errorf("unknown key %s", header.Kid)
}

func TestVariablesAreScopedAndSensitiveOnesAreSealed(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	owner := w.setup()
	w.project(owner, "a", "net", "prod")
	set := func(scope map[string]any, category, key, value string, sensitive bool) (map[string]any, string) {
		return owner.call("VariableService/SetVariable", map[string]any{"org": "a", "scope": scope, "category": category,
			"key": key, "value": value, "sensitive": sensitive})
	}
	org, project, ws := map[string]any{}, map[string]any{"project": "net"}, map[string]any{"project": "net", "workspace": "prod"}
	for _, c := range []struct {
		scope            map[string]any
		category, key, v string
		sensitive        bool
	}{
		{org, "VARIABLE_CATEGORY_TERRAFORM", "greeting", "hello-org", false},
		{org, "VARIABLE_CATEGORY_TERRAFORM", "team", "platform", false},
		{project, "VARIABLE_CATEGORY_TERRAFORM", "greeting", "hello-project", false},
		{ws, "VARIABLE_CATEGORY_ENV", "DB_PASSWORD", "hunter22-env", true},
	} {
		out, code := set(c.scope, c.category, c.key, c.v, c.sensitive)
		if code != "" {
			t.Fatalf("set %s: %s %v", c.key, code, out["message"])
		}
		if c.sensitive && strings.Contains(asJSON(out), c.v) {
			t.Errorf("SetVariable returned the sensitive value: %v", out)
		}
	}
	for _, c := range []struct{ category, key, value string }{
		{"VARIABLE_CATEGORY_ENV", "AWS_SECRET_ACCESS_KEY", "x"},
		{"VARIABLE_CATEGORY_ENV", "tf_log", "x"},
		{"VARIABLE_CATEGORY_ENV", "HOME", "x"},
		{"VARIABLE_CATEGORY_TERRAFORM", "has space", "x"},
	} {
		if _, code := set(ws, c.category, c.key, c.value, false); code != "invalid_argument" {
			t.Errorf("%s: %q, want invalid_argument", c.key, code)
		}
	}
	if _, code := set(ws, "VARIABLE_CATEGORY_TERRAFORM", "pin", "1234", true); code != "invalid_argument" {
		t.Errorf("a 4-character sensitive value: %q", code)
	}

	effective := owner.must("VariableService/ListVariables", map[string]any{"org": "a", "scope": ws, "effective": true})
	got := map[string]string{}
	for _, v := range asList(effective["variables"]) {
		m := v.(map[string]any)
		value, _ := m["value"].(string)
		scope, _ := m["scope"].(map[string]any)
		got[m["key"].(string)] = fmt.Sprintf("%s@%v", value, scope)
	}
	want := map[string]string{
		"greeting": "hello-project@map[project:net]", "team": "platform@map[]", "DB_PASSWORD": "@map[project:net workspace:prod]",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("effective variables: %v", got)
	}

	var value *string
	var sealed []byte
	if err := w.owner.QueryRow(context.Background(), `SELECT value, value_sealed FROM variables WHERE key = 'DB_PASSWORD'`).Scan(&value, &sealed); err != nil ||
		value != nil || len(sealed) == 0 || bytes.Contains(sealed, []byte("hunter22-env")) {
		t.Errorf("stored sensitive value: %v %q %v", value, sealed, err)
	}
	out := owner.must("VariableService/SetVariable", map[string]any{"org": "a", "scope": ws, "category": "VARIABLE_CATEGORY_ENV",
		"key": "DB_PASSWORD", "sensitive": true, "keepValue": true, "description": "the app's database"})
	var after []byte
	if err := w.owner.QueryRow(context.Background(), `SELECT value_sealed FROM variables WHERE key = 'DB_PASSWORD'`).Scan(&after); err != nil ||
		!bytes.Equal(after, sealed) || out["variable"].(map[string]any)["description"] != "the app's database" {
		t.Errorf("keep the value: %v %v", out, err)
	}
	owner.must("VariableService/DeleteVariable", map[string]any{"org": "a", "scope": project, "category": "VARIABLE_CATEGORY_TERRAFORM", "key": "greeting"})
	effective = owner.must("VariableService/ListVariables", map[string]any{"org": "a", "scope": ws, "effective": true})
	if !strings.Contains(asJSON(effective), "hello-org") {
		t.Errorf("after the project's greeting went: %v", effective)
	}
}

func TestCloudAccountTrustNamesItsWorkspaces(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	owner := w.setup()
	w.project(owner, "a", "net", "prod", "dev")
	account := func(plan, apply, region string) string {
		_, code := owner.call("CloudAccountService/CreateCloudAccount", map[string]any{"org": "a", "name": "test",
			"aws": map[string]any{"planRoleArn": plan, "applyRoleArn": apply, "region": region}})
		return code
	}
	for _, c := range [][3]string{
		{"arn:aws:iam::123456789012:role/plan", "arn:aws:iam::123456789012:role/plan", "eu-west-1"},
		{"arn:aws:iam::123456789012:role/plan", "arn:aws:iam::210987654321:role/apply", "eu-west-1"},
		{"arn:aws-cn:iam::123456789012:role/plan", "arn:aws-cn:iam::123456789012:role/apply", "eu-west-1"},
		{"arn:aws:iam::123456789012:role/plan", "arn:aws:iam::123456789012:role/apply", "europe"},
	} {
		if code := account(c[0], c[1], c[2]); code != "invalid_argument" {
			t.Errorf("%v: %q", c, code)
		}
	}
	if code := account("arn:aws:iam::123456789012:role/sw/plan", "arn:aws:iam::123456789012:role/sw/apply", "eu-west-1"); code != "" {
		t.Fatalf("create: %s", code)
	}
	if _, code := owner.call("CloudAccountService/GetAwsSetup", map[string]any{"org": "a", "account": "test"}); code != "failed_precondition" {
		t.Errorf("setup with no workspace: %q", code)
	}
	owner.must("CloudAccountService/SetWorkspaceCloudAccount", map[string]any{"org": "a", "project": "net", "workspace": "prod", "account": "test"})
	setup := owner.must("CloudAccountService/GetAwsSetup", map[string]any{"org": "a", "account": "test"})
	orgID, prod := w.workspaceIDs("net", "prod")
	_, dev := w.workspaceIDs("net", "dev")
	plan, apply := setup["planRoleTrustPolicy"].(string), setup["applyRoleTrustPolicy"].(string)
	if !strings.Contains(plan, oidc.Subject(orgID, prod, "plan")) || strings.Contains(plan, "phase:apply") || strings.Contains(plan, dev) ||
		!strings.Contains(apply, oidc.Subject(orgID, prod, "apply")) || !strings.Contains(plan, `"stackwright.test:aud": "sts.amazonaws.com"`) {
		t.Errorf("trust policies:\n%s\n%s", plan, apply)
	}
	template := setup["cloudformationTemplate"].(string)
	if !strings.Contains(template, "RoleName: plan\n      Path: /sw/") || !strings.Contains(template, "Url: "+testIssuer) {
		t.Errorf("template:\n%s", template)
	}
	if _, code := owner.call("CloudAccountService/DeleteCloudAccount", map[string]any{"org": "a", "name": "test"}); code != "failed_precondition" {
		t.Errorf("delete an account in use: %q", code)
	}
	owner.must("CloudAccountService/SetWorkspaceCloudAccount", map[string]any{"org": "a", "project": "net", "workspace": "prod"})
	owner.must("CloudAccountService/DeleteCloudAccount", map[string]any{"org": "a", "name": "test"})

	for path, want := range map[string]string{
		"/.well-known/openid-configuration": `"jwks_uri": "` + testIssuer + `/.well-known/jwks.json"`,
		"/.well-known/jwks.json":            `"alg": "RS256"`,
	} {
		resp, err := http.Get(w.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if !strings.Contains(string(b), want) {
			t.Errorf("%s: %s", path, b)
		}
	}
}

func (w *world) workspaceIDs(project, workspace string) (orgID, id string) {
	w.t.Helper()
	if err := w.owner.QueryRow(context.Background(), `SELECT w.org_id, w.id FROM workspaces w JOIN projects p ON p.id = w.project_id
		WHERE p.slug = $1 AND w.name = $2`, project, workspace).Scan(&orgID, &id); err != nil {
		w.t.Fatal(err)
	}
	return orgID, id
}

func TestRunGetsVariablesAndCloudCredentials(t *testing.T) {
	t.Parallel()
	requireDocker(t)
	w := start(t, nil, true)
	owner := w.setup()
	w.project(owner, "a", "net", "prod", "other")
	w.startBuilder(2)
	rw := &runWorld{world: w, owner: owner}
	const secret = "s3cr3t-value-123"
	prod := map[string]any{"org": "a", "project": "net", "workspace": "prod"}
	other := map[string]any{"org": "a", "project": "net", "workspace": "other"}
	for _, v := range []map[string]any{
		{"scope": map[string]any{}, "category": "VARIABLE_CATEGORY_TERRAFORM", "key": "greeting", "value": "hello-org"},
		{"scope": map[string]any{"project": "net"}, "category": "VARIABLE_CATEGORY_TERRAFORM", "key": "greeting", "value": "hello-project"},
		{"scope": map[string]any{"project": "net"}, "category": "VARIABLE_CATEGORY_TERRAFORM", "key": "secret", "value": secret, "sensitive": true},
		{"scope": map[string]any{"project": "net"}, "category": "VARIABLE_CATEGORY_ENV", "key": "DB_PASSWORD", "value": "hunter22-env", "sensitive": true},
	} {
		owner.must("VariableService/SetVariable", merge(v, map[string]any{"org": "a"}))
	}
	planRole, applyRole := "arn:aws:iam::123456789012:role/sw-plan", "arn:aws:iam::123456789012:role/sw-apply"
	owner.must("CloudAccountService/CreateCloudAccount", map[string]any{"org": "a", "name": "test",
		"aws": map[string]any{"planRoleArn": planRole, "applyRoleArn": applyRole, "region": "eu-west-1"}})
	owner.must("CloudAccountService/SetWorkspaceCloudAccount", merge(prod, map[string]any{"account": "test"}))
	setup := owner.must("CloudAccountService/GetAwsSetup", map[string]any{"org": "a", "account": "test"})
	w.sts.allow(planRole, setup["planRoleTrustPolicy"].(string))
	w.sts.allow(applyRole, setup["applyRoleTrustPolicy"].(string))

	upload := map[string]any{"source": base64.StdEncoding.EncodeToString(source(t, "secrets")), "sourceName": "secrets.tar.gz"}
	owner.must("RunService/CreateRun", merge(prod, upload))
	if run := rw.waitFor(prod, 1, "RUN_STATUS_NEEDS_APPROVAL", "RUN_STATUS_FAILED"); run["status"] != "RUN_STATUS_NEEDS_APPROVAL" {
		t.Fatalf("plan: %v", run)
	}
	owner.must("RunService/ApproveRun", merge(prod, map[string]any{"number": 1}))
	run := rw.waitFor(prod, 1, "RUN_STATUS_APPLIED", "RUN_STATUS_FAILED")
	if run["status"] != "RUN_STATUS_APPLIED" {
		t.Fatalf("apply: %v", run)
	}

	orgID, prodID := w.workspaceIDs("net", "prod")
	got := w.sts.all()
	if len(got) != 2 || got[0].role != planRole || got[0].sub != oidc.Subject(orgID, prodID, "plan") ||
		got[1].role != applyRole || got[1].sub != oidc.Subject(orgID, prodID, "apply") {
		t.Fatalf("issued: %+v", got)
	}

	// Everything the run left behind, as an attacker with read access would see it.
	var seen bytes.Buffer
	rows, err := w.owner.Query(context.Background(), `SELECT e.type, e.payload::text FROM job_events e JOIN jobs j ON j.id = e.job_id WHERE j.run_id = $1
		UNION ALL SELECT type, payload::text FROM run_events WHERE run_id = $1`, run["id"])
	if err != nil {
		t.Fatal(err)
	}
	var credentials int
	for rows.Next() {
		var typ, payload string
		if err := rows.Scan(&typ, &payload); err != nil {
			t.Fatal(err)
		}
		if typ == "executor.credentials_issued" {
			credentials++
		}
		seen.WriteString(payload + "\n")
	}
	rows.Close()
	if credentials != 2 {
		t.Errorf("credentials_issued events: %d", credentials)
	}
	var planJob, applyJob string
	if err := w.owner.QueryRow(context.Background(), `SELECT plan_job_id, apply_job_id FROM runs WHERE id = $1`, run["id"]).Scan(&planJob, &applyJob); err != nil {
		t.Fatal(err)
	}
	jw := &jobWorld{world: w}
	seen.Write(jw.artifact(planJob, "plan.json"))
	rows, err = w.owner.Query(context.Background(), `SELECT payload::text FROM job_events WHERE job_id = ANY($1) ORDER BY job_id, seq`,
		[]string{planJob, applyJob})
	if err != nil {
		t.Fatal(err)
	}
	events, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	seen.WriteString(strings.Join(events, "\n"))
	seen.WriteString(asJSON(owner.must("RunService/GetRun", merge(prod, map[string]any{"number": 1}))))
	seen.WriteString(asJSON(owner.must("VariableService/ListVariables", map[string]any{"org": "a", "scope": map[string]any{"project": "net"}})))
	if !strings.Contains(seen.String(), "hello-project:(sensitive value)") {
		t.Errorf("the plan does not show the masked input; the variables may not have reached the engine")
	}
	for _, s := range []string{secret, "hunter22-env", stsSecretKey, stsSessionToken, got[0].token, got[1].token} {
		if strings.Contains(seen.String(), s) {
			t.Errorf("%q leaked into events, artifacts or the API", s)
		}
	}

	// A token for the plan phase cannot assume the apply role.
	resp, err := http.PostForm(strings.Replace(w.sts.url, "host.docker.internal", "127.0.0.1", 1), map[string][]string{
		"Action": {"AssumeRoleWithWebIdentity"}, "RoleArn": {applyRole}, "WebIdentityToken": {got[0].token}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a plan token on the apply role: %d", resp.StatusCode)
	}

	// Another workspace on the same account, whose subject the trust policies
	// do not list yet, is refused.
	owner.must("CloudAccountService/SetWorkspaceCloudAccount", merge(other, map[string]any{"account": "test"}))
	owner.must("RunService/CreateRun", merge(other, upload))
	failed := rw.waitFor(other, 1, "RUN_STATUS_NEEDS_APPROVAL", "RUN_STATUS_FAILED")
	if failed["status"] != "RUN_STATUS_FAILED" || !strings.Contains(fmt.Sprint(failed["error"]), "AccessDenied") {
		t.Errorf("a workspace that the trust policy does not name: %v", failed)
	}
}

func TestProjectStartsWithACloudAccount(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	owner := w.setup()
	owner.must("CloudAccountService/CreateCloudAccount", map[string]any{"org": "a", "name": "test",
		"aws": map[string]any{"planRoleArn": "arn:aws:iam::123456789012:role/plan", "applyRoleArn": "arn:aws:iam::123456789012:role/apply", "region": "eu-west-1"}})
	project := map[string]any{"org": "a", "slug": "net", "engine": "ENGINE_OPENTOFU", "engineVersion": "1.10.7",
		"layout": "LAYOUT_CLI_WORKSPACES", "workspaces": []string{"dev", "prod"}}
	if _, code := owner.call("ProjectService/CreateProject", merge(project, map[string]any{"cloudAccount": "missing"})); code != "not_found" {
		t.Errorf("an unknown account: %q", code)
	}
	owner.must("ProjectService/CreateProject", merge(project, map[string]any{"cloudAccount": "test"}))
	for _, ws := range []string{"dev", "prod"} {
		got := owner.must("ProjectService/GetWorkspace", map[string]any{"org": "a", "project": "net", "workspace": ws})["workspace"].(map[string]any)
		if got["cloudAccount"] != "test" {
			t.Errorf("%s: %v", ws, got)
		}
	}
}

func TestEngineVersionsComeFromTheReleases(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	owner := w.setup()
	for engine, want := range map[string][2]string{"ENGINE_TERRAFORM": {"1.17.2", "1.5.7"}, "ENGINE_OPENTOFU": {"1.13.9", "1.10.7"}} {
		got := owner.must("ProjectService/ListEngineVersions", map[string]any{"engine": engine})["versions"].([]any)
		list := fmt.Sprint(got)
		if !strings.Contains(list, want[0]) || !strings.Contains(list, want[1]) || strings.Contains(list, "-") {
			t.Errorf("%s: %v", engine, got)
		}
		if engine == "ENGINE_OPENTOFU" && strings.Contains(list, " 1.7.") {
			t.Errorf("OpenTofu below 1.8.0 is listed: %v", got)
		}
	}
	if _, code := owner.call("ProjectService/CreateProject", map[string]any{"org": "a", "slug": "tf117", "engine": "ENGINE_TERRAFORM",
		"engineVersion": "1.17.2", "layout": "LAYOUT_PLAIN"}); code != "" {
		t.Errorf("Terraform 1.17.2: %q", code)
	}
}
