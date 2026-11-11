package server_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/internal/auth"
	"github.com/stackwright/stackwright/internal/authz"
	"github.com/stackwright/stackwright/internal/seal"
	"github.com/stackwright/stackwright/internal/server"
	"github.com/stackwright/stackwright/internal/testenv"
)

const (
	setupCode = "setup123"
	password  = "correct horse battery"
	issuer    = "http://127.0.0.1:18071/default"
)

type world struct {
	t     *testing.T
	srv   *httptest.Server
	owner *pgxpool.Pool
}

func newWorld(t *testing.T, web fstest.MapFS) *world {
	d := testenv.New(t)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	kek, err := seal.ParseKEK(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(nil)
	srv.Start()
	t.Cleanup(srv.Close)
	cfg := server.Config{Pool: d.App, KEK: kek, PublicURL: srv.URL, SetupCode: setupCode}
	if web != nil {
		cfg.Web = web
	}
	srv.Config.Handler = server.New(cfg)
	return &world{t: t, srv: srv, owner: d.Owner}
}

type client struct {
	w    *world
	http *http.Client
}

func (w *world) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{w: w, http: &http.Client{Jar: jar}}
}

func (c *client) call(procedure string, body any) (map[string]any, string) {
	c.w.t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, c.w.srv.URL+"/stackwright.v1."+procedure, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	u, _ := url.Parse(c.w.srv.URL)
	for _, ck := range c.http.Jar.Cookies(u) {
		if ck.Name == auth.CSRFCookie {
			req.Header.Set(auth.CSRFHeader, ck.Value)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.w.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out := map[string]any{}
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &out)
	if resp.StatusCode != http.StatusOK {
		code, _ := out["code"].(string)
		return out, code
	}
	return out, ""
}

func (c *client) must(procedure string, body any) map[string]any {
	c.w.t.Helper()
	out, code := c.call(procedure, body)
	if code != "" {
		c.w.t.Fatalf("%s: %s %v", procedure, code, out["message"])
	}
	return out
}

func (w *world) setup() *client {
	c := w.client()
	c.must("AuthService/CompleteSetup", map[string]any{
		"setupCode": setupCode, "orgName": "Acme", "orgSlug": "a", "email": "owner@a.test", "name": "Owner", "password": password,
	})
	return c
}

func (w *world) invite(by *client, org, email, role string) string {
	w.t.Helper()
	out := by.must("OrgService/CreateInvite", map[string]any{"org": org, "email": email, "role": role})
	u, _ := out["url"].(string)
	return u[strings.LastIndex(u, "/")+1:]
}

func (w *world) member(by *client, org, email, role string) *client {
	w.t.Helper()
	c := w.client()
	c.must("AuthService/AcceptInvite", map[string]any{"token": w.invite(by, org, email, role), "name": email, "password": password})
	return c
}

func (w *world) auditRows() int {
	var n int
	if err := w.owner.QueryRow(context.Background(), `SELECT count(*) FROM audit_log`).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func (w *world) userID(email string) string {
	var id string
	if err := w.owner.QueryRow(context.Background(), `SELECT id FROM users WHERE email = $1`, email).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	return id
}

type method struct {
	procedure string
	write     bool
}

func methods() []method {
	var out []method
	for _, f := range []protoreflect.FileDescriptor{
		v1.File_stackwright_v1_auth_proto, v1.File_stackwright_v1_orgs_proto,
		v1.File_stackwright_v1_instance_proto, v1.File_stackwright_v1_meta_proto,
	} {
		for i := range f.Services().Len() {
			s := f.Services().Get(i)
			for j := range s.Methods().Len() {
				m := s.Methods().Get(j)
				level := m.Options().(*descriptorpb.MethodOptions).GetIdempotencyLevel()
				out = append(out, method{"/" + string(s.FullName()) + "/" + string(m.Name()), level != descriptorpb.MethodOptions_NO_SIDE_EFFECTS})
			}
		}
	}
	return out
}

func short(procedure string) string {
	return strings.TrimPrefix(procedure, "/stackwright.v1.")
}

func asJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestEveryProcedureHasAPolicy(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range methods() {
		seen[m.procedure] = true
		if _, ok := authz.Policy[m.procedure]; !ok {
			t.Errorf("%s has no policy", m.procedure)
		}
	}
	for p := range authz.Policy {
		if !seen[p] {
			t.Errorf("policy names %s, which is not an RPC", p)
		}
	}
}

func TestHealthAndWebApp(t *testing.T) {
	w := newWorld(t, fstest.MapFS{
		"index.html":    {Data: []byte("<div id=root>")},
		"assets/app.js": {Data: []byte("app")},
	})
	get := func(method, path string) (int, http.Header, string) {
		req, _ := http.NewRequest(method, w.srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, string(b)
	}
	if code, header, body := get(http.MethodGet, "/healthz"); code != 200 || body != "ok\n" ||
		!strings.Contains(header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("healthz = %d %q, CSP %q", code, body, header.Get("Content-Security-Policy"))
	}
	if _, _, body := get(http.MethodGet, "/assets/app.js"); body != "app" {
		t.Errorf("asset = %q", body)
	}
	if _, _, body := get(http.MethodGet, "/a/settings/members"); !strings.Contains(body, "root") {
		t.Errorf("deep link = %q, want index.html", body)
	}
	if code, _, _ := get(http.MethodPost, "/a"); code != http.StatusMethodNotAllowed {
		t.Errorf("POST to the app = %d", code)
	}
}

func TestSetupNeedsTheCodeAndWorksOnce(t *testing.T) {
	w := newWorld(t, nil)
	c := w.client()
	if out := c.must("AuthService/GetSetup", map[string]any{}); out["needed"] != true {
		t.Fatalf("GetSetup = %v, want needed", out)
	}
	body := map[string]any{"setupCode": "wrong", "orgName": "Acme", "orgSlug": "a", "email": "owner@a.test", "password": password}
	if _, code := c.call("AuthService/CompleteSetup", body); code != "permission_denied" {
		t.Fatalf("wrong code: %s, want permission_denied", code)
	}
	var users int
	_ = w.owner.QueryRow(context.Background(), `SELECT count(*) FROM users`).Scan(&users)
	if users != 0 {
		t.Fatalf("a wrong code made %d users", users)
	}
	body["setupCode"] = setupCode
	c.must("AuthService/CompleteSetup", body)
	me := c.must("AuthService/GetMe", map[string]any{})
	if me["user"].(map[string]any)["instanceAdmin"] != true {
		t.Errorf("the first user is not an instance admin: %v", me)
	}
	body["email"] = "second@a.test"
	if _, code := w.client().call("AuthService/CompleteSetup", body); code != "failed_precondition" {
		t.Fatalf("second setup: %s, want failed_precondition", code)
	}
	if out := c.must("AuthService/GetSetup", map[string]any{}); out["needed"] == true {
		t.Fatal("GetSetup still says needed")
	}
}

func TestSignInAndOut(t *testing.T) {
	w := newWorld(t, nil)
	w.setup()
	c := w.client()
	if _, code := c.call("AuthService/SignIn", map[string]any{"email": "owner@a.test", "password": "wrong password!"}); code != "unauthenticated" {
		t.Fatalf("wrong password: %s", code)
	}
	if _, code := c.call("AuthService/SignIn", map[string]any{"email": "nobody@a.test", "password": password}); code != "unauthenticated" {
		t.Fatalf("unknown email: %s", code)
	}
	c.must("AuthService/SignIn", map[string]any{"email": "Owner@A.test", "password": password})
	c.must("AuthService/GetMe", map[string]any{})
	c.must("AuthService/SignOut", map[string]any{})
	if _, code := c.call("AuthService/GetMe", map[string]any{}); code != "unauthenticated" {
		t.Fatalf("after sign-out GetMe = %s", code)
	}
}

func TestAWrongCSRFHeaderCountsAsSignedOut(t *testing.T) {
	w := newWorld(t, nil)
	c := w.setup()
	u, _ := url.Parse(w.srv.URL)
	var cookies []string
	for _, ck := range c.http.Jar.Cookies(u) {
		cookies = append(cookies, ck.Name+"="+ck.Value)
	}
	send := func(csrf, origin string) int {
		req, _ := http.NewRequest(http.MethodPost, w.srv.URL+"/stackwright.v1.AuthService/GetMe", strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Cookie", strings.Join(cookies, "; "))
		req.Header.Set(auth.CSRFHeader, csrf)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := send("forged", ""); code != http.StatusUnauthorized {
		t.Errorf("forged CSRF header: %d", code)
	}
	if code := send("", "https://evil.example"); code != http.StatusForbidden {
		t.Errorf("cross-origin request: %d", code)
	}
}

func TestRoleMatrix(t *testing.T) {
	w := newWorld(t, nil)
	owner := w.setup()
	as := map[v1.Role]*client{v1.Role_ROLE_OWNER: owner}
	for _, r := range []v1.Role{v1.Role_ROLE_ADMIN, v1.Role_ROLE_WRITER, v1.Role_ROLE_REVIEWER, v1.Role_ROLE_SECURITY, v1.Role_ROLE_VIEWER} {
		as[r] = w.member(owner, "a", authz.RoleToDB(r)+"@a.test", r.String())
	}
	anonymous := w.client()

	for _, m := range methods() {
		rule := authz.Policy[m.procedure]
		switch rule.Access {
		case authz.Public:
			if _, code := anonymous.call(short(m.procedure), map[string]any{}); code == "permission_denied" {
				t.Errorf("%s refused an anonymous caller", m.procedure)
			}
		case authz.SignedIn:
			if _, code := anonymous.call(short(m.procedure), map[string]any{}); code != "unauthenticated" {
				t.Errorf("%s anonymous: %s, want unauthenticated", m.procedure, code)
			}
		case authz.InstanceAdmin:
			if _, code := as[v1.Role_ROLE_ADMIN].call(short(m.procedure), map[string]any{}); code != "permission_denied" {
				t.Errorf("%s as a non-admin: %s, want permission_denied", m.procedure, code)
			}
			if _, code := owner.call(short(m.procedure), map[string]any{}); code == "permission_denied" || code == "unauthenticated" {
				t.Errorf("%s as the instance admin: %s", m.procedure, code)
			}
		case authz.InOrg:
			for role, c := range as {
				_, code := c.call(short(m.procedure), map[string]any{"org": "a"})
				if denied, want := code == "permission_denied", !authz.AtLeast(role, rule.Min); denied != want {
					t.Errorf("%s as %s: %q, want denied=%v", m.procedure, role, code, want)
				}
			}
			if _, code := anonymous.call(short(m.procedure), map[string]any{"org": "a"}); code != "unauthenticated" {
				t.Errorf("%s anonymous: %s, want unauthenticated", m.procedure, code)
			}
		default:
			t.Errorf("%s has access %v", m.procedure, rule.Access)
		}
	}
}

func TestAnotherOrgIsNotFound(t *testing.T) {
	w := newWorld(t, nil)
	owner := w.setup()
	adminA := w.member(owner, "a", "admin@a.test", "ROLE_ADMIN")
	owner.must("OrgService/CreateOrg", map[string]any{"slug": "b", "name": "B"})
	w.member(owner, "b", "member@b.test", "ROLE_WRITER")
	inviteB := owner.must("OrgService/CreateInvite", map[string]any{"org": "b", "email": "open@b.test", "role": "ROLE_VIEWER"})
	inviteBID := inviteB["invite"].(map[string]any)["id"]
	memberB := w.userID("member@b.test")

	for _, m := range methods() {
		if authz.Policy[m.procedure].Access != authz.InOrg {
			continue
		}
		if _, code := adminA.call(short(m.procedure), map[string]any{"org": "b"}); code != "not_found" {
			t.Errorf("%s on org b: %s, want not_found", m.procedure, code)
		}
	}
	for _, call := range []struct {
		procedure string
		body      map[string]any
	}{
		{"OrgService/SetMemberRole", map[string]any{"org": "a", "userId": memberB, "role": "ROLE_VIEWER"}},
		{"OrgService/RemoveMember", map[string]any{"org": "a", "userId": memberB}},
		{"OrgService/RevokeInvite", map[string]any{"org": "a", "id": inviteBID}},
	} {
		if _, code := adminA.call(call.procedure, call.body); code != "not_found" {
			t.Errorf("%s with org b's object: %s, want not_found", call.procedure, code)
		}
	}
	if members := adminA.must("OrgService/ListMembers", map[string]any{"org": "a"}); strings.Contains(asJSON(members), "member@b.test") {
		t.Error("org a's member list shows org b's member")
	}
	if events := asJSON(adminA.must("OrgService/ListAuditEvents", map[string]any{"org": "a"})); strings.Contains(events, "org:b") {
		t.Error("org a's audit log shows org b's events")
	}
}

func TestEveryWriteMakesOneAuditRow(t *testing.T) {
	w := newWorld(t, nil)
	covered := map[string]bool{}
	step := func(procedure string, run func()) {
		t.Helper()
		before := w.auditRows()
		run()
		if got := w.auditRows() - before; got != 1 {
			t.Errorf("%s made %d audit rows, want 1", procedure, got)
		}
		covered["/stackwright.v1."+procedure] = true
	}

	owner := w.client()
	step("AuthService/CompleteSetup", func() {
		owner.must("AuthService/CompleteSetup", map[string]any{
			"setupCode": setupCode, "orgName": "Acme", "orgSlug": "a", "email": "owner@a.test", "password": password,
		})
	})
	var token string
	step("OrgService/CreateInvite", func() {
		u := owner.must("OrgService/CreateInvite", map[string]any{"org": "a", "email": "new@a.test", "role": "ROLE_WRITER"})["url"].(string)
		token = u[strings.LastIndex(u, "/")+1:]
	})
	step("AuthService/AcceptInvite", func() {
		w.client().must("AuthService/AcceptInvite", map[string]any{"token": token, "name": "New", "password": password})
	})
	step("OrgService/SetMemberRole", func() {
		owner.must("OrgService/SetMemberRole", map[string]any{"org": "a", "userId": w.userID("new@a.test"), "role": "ROLE_ADMIN"})
	})
	step("OrgService/RemoveMember", func() {
		owner.must("OrgService/RemoveMember", map[string]any{"org": "a", "userId": w.userID("new@a.test")})
	})
	out := owner.must("OrgService/CreateInvite", map[string]any{"org": "a", "email": "gone@a.test", "role": "ROLE_VIEWER"})
	step("OrgService/RevokeInvite", func() {
		owner.must("OrgService/RevokeInvite", map[string]any{"org": "a", "id": out["invite"].(map[string]any)["id"]})
	})
	step("OrgService/CreateOrg", func() {
		owner.must("OrgService/CreateOrg", map[string]any{"slug": "b", "name": "B"})
	})
	var providerID string
	step("InstanceService/CreateSignInProvider", func() {
		p := owner.must("InstanceService/CreateSignInProvider", map[string]any{
			"kind": "SIGN_IN_PROVIDER_KIND_GITHUB", "name": "GitHub", "clientId": "id", "clientSecret": "secret",
		})
		providerID = p["provider"].(map[string]any)["id"].(string)
	})
	step("InstanceService/DeleteSignInProvider", func() {
		owner.must("InstanceService/DeleteSignInProvider", map[string]any{"id": providerID})
	})
	second := w.client()
	step("AuthService/SignIn", func() {
		second.must("AuthService/SignIn", map[string]any{"email": "owner@a.test", "password": password})
	})
	step("AuthService/SignOut", func() {
		second.must("AuthService/SignOut", map[string]any{})
	})

	before := w.auditRows()
	if _, code := owner.call("OrgService/CreateOrg", map[string]any{"slug": "b", "name": "B again"}); code != "already_exists" {
		t.Errorf("duplicate org: %s", code)
	}
	if w.auditRows() != before {
		t.Error("a failed write made an audit row")
	}

	for _, m := range methods() {
		if m.write && !covered[m.procedure] {
			t.Errorf("%s writes, but this test does not check its audit row", m.procedure)
		}
	}
}

func TestOwnerRules(t *testing.T) {
	w := newWorld(t, nil)
	owner := w.setup()
	admin := w.member(owner, "a", "admin@a.test", "ROLE_ADMIN")
	ownerID := w.userID("owner@a.test")
	if _, code := admin.call("OrgService/SetMemberRole", map[string]any{"org": "a", "userId": w.userID("admin@a.test"), "role": "ROLE_OWNER"}); code != "permission_denied" {
		t.Errorf("an admin made themself owner: %s", code)
	}
	if _, code := admin.call("OrgService/RemoveMember", map[string]any{"org": "a", "userId": ownerID}); code != "permission_denied" {
		t.Errorf("an admin removed the owner: %s", code)
	}
	if _, code := admin.call("OrgService/CreateInvite", map[string]any{"org": "a", "email": "o2@a.test", "role": "ROLE_OWNER"}); code != "permission_denied" {
		t.Errorf("an admin invited an owner: %s", code)
	}
	if _, code := owner.call("OrgService/SetMemberRole", map[string]any{"org": "a", "userId": ownerID, "role": "ROLE_ADMIN"}); code != "failed_precondition" {
		t.Errorf("the last owner stepped down: %s", code)
	}
}

func TestInvites(t *testing.T) {
	w := newWorld(t, nil)
	owner := w.setup()

	token := w.invite(owner, "a", "kim@a.test", "ROLE_VIEWER")
	out := w.client().must("AuthService/GetInvite", map[string]any{"token": token})
	if out["orgName"] != "Acme" || out["email"] != "kim@a.test" || out["hasAccount"] == true {
		t.Errorf("GetInvite = %v", out)
	}
	kim := w.client()
	if _, code := kim.call("AuthService/AcceptInvite", map[string]any{"token": token, "password": "short"}); code != "invalid_argument" {
		t.Errorf("weak password: %s", code)
	}
	kim.must("AuthService/AcceptInvite", map[string]any{"token": token, "name": "Kim", "password": password})
	if _, code := w.client().call("AuthService/AcceptInvite", map[string]any{"token": token, "password": password}); code != "not_found" {
		t.Errorf("an invite worked twice: %s", code)
	}

	owner.must("OrgService/CreateOrg", map[string]any{"slug": "b", "name": "B"})
	again := w.invite(owner, "b", "kim@a.test", "ROLE_VIEWER")
	if _, code := w.client().call("AuthService/AcceptInvite", map[string]any{"token": again, "password": password}); code != "failed_precondition" {
		t.Errorf("an existing account without signing in: %s", code)
	}
	if _, code := owner.call("AuthService/AcceptInvite", map[string]any{"token": again}); code != "permission_denied" {
		t.Errorf("another user took kim's invite: %s", code)
	}
	kim.must("AuthService/AcceptInvite", map[string]any{"token": again})
	if n := len(kim.must("AuthService/GetMe", map[string]any{})["orgs"].([]any)); n != 2 {
		t.Errorf("kim is in %d orgs, want 2", n)
	}

	revoked := owner.must("OrgService/CreateInvite", map[string]any{"org": "a", "email": "lee@a.test", "role": "ROLE_VIEWER"})
	owner.must("OrgService/RevokeInvite", map[string]any{"org": "a", "id": revoked["invite"].(map[string]any)["id"]})
	u := revoked["url"].(string)
	if _, code := w.client().call("AuthService/AcceptInvite", map[string]any{"token": u[strings.LastIndex(u, "/")+1:], "password": password}); code != "not_found" {
		t.Errorf("a revoked invite worked: %s", code)
	}
}

func signInWithMock(t *testing.T, c *client, start, claims string) *url.URL {
	t.Helper()
	resp, err := c.http.Get(start)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !strings.HasSuffix(resp.Request.URL.Path, "/authorize") {
		t.Fatalf("sign-in did not reach the provider: %s", resp.Request.URL)
	}
	resp, err = c.http.PostForm(resp.Request.URL.String(), url.Values{"username": {"kim"}, "claims": {claims}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.Request.URL
}

func TestOIDCSignInIsByInviteOnly(t *testing.T) {
	w := newWorld(t, nil)
	owner := w.setup()
	out, code := owner.call("InstanceService/CreateSignInProvider", map[string]any{
		"kind": "SIGN_IN_PROVIDER_KIND_OIDC", "name": "Dev", "issuer": issuer, "clientId": "stackwright", "clientSecret": "dev-client-secret",
	})
	if code != "" {
		t.Fatalf("create provider: %s %v (is the mock OIDC server in compose.dev.yaml up?)", code, out["message"])
	}
	id := out["provider"].(map[string]any)["id"].(string)
	start := w.srv.URL + "/auth/oidc/" + id + "/start"
	verified := `{"email":"kim@a.test","email_verified":true,"name":"Kim"}`

	if u := signInWithMock(t, w.client(), start, verified); u.Path != "/sign-in" || u.Query().Get("error") != "no-account" {
		t.Errorf("no account, no invite: landed on %s", u)
	}
	if u := signInWithMock(t, w.client(), start, `{"email":"kim@a.test","email_verified":false}`); u.Query().Get("error") != "email" {
		t.Errorf("unverified email: landed on %s", u)
	}

	token := w.invite(owner, "a", "kim@a.test", "ROLE_WRITER")
	kim := w.client()
	if u := signInWithMock(t, kim, start+"?invite="+token, verified); u.Path != "/a" {
		t.Fatalf("with an invite: landed on %s", u)
	}
	if me := kim.must("AuthService/GetMe", map[string]any{}); !strings.Contains(asJSON(me), `"slug":"a"`) {
		t.Errorf("kim is not in org a: %v", me)
	}
	if u := signInWithMock(t, w.client(), start, verified); u.Path != "/" {
		t.Errorf("a linked identity: landed on %s", u)
	}

	var stored []byte
	if err := w.owner.QueryRow(context.Background(), `SELECT client_secret FROM sign_in_providers WHERE id = $1`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte("dev-client-secret")) {
		t.Error("the client secret is stored in plain text")
	}
}
