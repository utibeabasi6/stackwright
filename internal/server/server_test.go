package server_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/internal/agent"
	"github.com/stackwright/stackwright/internal/auth"
	"github.com/stackwright/stackwright/internal/authz"
	"github.com/stackwright/stackwright/internal/builder"
	"github.com/stackwright/stackwright/internal/builders"
	"github.com/stackwright/stackwright/internal/engine"
	"github.com/stackwright/stackwright/internal/github"
	"github.com/stackwright/stackwright/internal/jobs"
	"github.com/stackwright/stackwright/internal/oidc"
	"github.com/stackwright/stackwright/internal/runs"
	"github.com/stackwright/stackwright/internal/seal"
	"github.com/stackwright/stackwright/internal/server"
	"github.com/stackwright/stackwright/internal/state"
	"github.com/stackwright/stackwright/internal/testenv"
)

const (
	setupCode = "setup123"
	password  = "correct horse battery"
	issuer    = "http://127.0.0.1:18071/default"

	builderToken  = "builder-test-token"
	testIssuer    = "https://stackwright.test"
	serverVersion = "v0.6.0"
)

type world struct {
	t      *testing.T
	srv    *httptest.Server
	owner  *pgxpool.Pool
	s3     *minio.Client
	bucket string
	public string
	jobs   *jobs.Service
	runs   *runs.Orchestrator
	hub    *builders.Hub
	// builderLocal is the builder port as the test process reaches it.
	builderLocal string
	sts          *fakeSTS
	github       *fakeGitHub
	agent        *agent.Service
	model        *scriptedModel
	cfg          server.Config
	// handler and current let restartAgent swap in a new agent service.
	handler *atomic.Pointer[http.Handler]
	current *atomic.Pointer[agent.Service]
}

func newWorld(t *testing.T, web fstest.MapFS) *world {
	return start(t, web, false)
}

// start with engines listens on every interface, so containers can reach the
// server through host.docker.internal.
func start(t *testing.T, web fstest.MapFS, engines bool) *world {
	return startWith(t, web, engines, nil)
}

// startWith hands out binaries from the builder port. Without them, it hands
// out an empty file, which is enough to make a join command.
func startWith(t *testing.T, web fstest.MapFS, engines bool, binaries map[string]builders.Binary) *world {
	if binaries == nil {
		empty := builders.Binary{Path: "/dev/null", SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}
		binaries = map[string]builders.Binary{"linux_amd64": empty, "linux_arm64": empty}
	}
	d := testenv.New(t)
	s3, bucket := testenv.S3(t)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	kek, err := seal.ParseKEK(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(nil)
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	public := ""
	if engines {
		l, err := net.Listen("tcp", "0.0.0.0:0")
		if err != nil {
			t.Fatal(err)
		}
		srv.Listener = l
		public = fmt.Sprintf("http://host.docker.internal:%d", l.Addr().(*net.TCPAddr).Port)
	}
	srv.Start()
	t.Cleanup(srv.Close)
	if public == "" {
		public = srv.URL
	}
	presign, err := minio.New("host.docker.internal:19000", &minio.Options{
		Creds:  credentials.NewStaticV4(os.Getenv("STACKWRIGHT_S3_ACCESS_KEY"), os.Getenv("STACKWRIGHT_S3_SECRET_KEY"), ""),
		Region: "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := oidc.Load(context.Background(), d.App, kek, testIssuer)
	if err != nil {
		t.Fatal(err)
	}
	sts := newFakeSTS(t, srv)
	js := &jobs.Service{Pool: d.App, KEK: kek, S3: s3, Presign: presign, PresignRemote: presign, Bucket: bucket,
		Engines: &engine.Mirror{Pool: d.App, S3: s3, Bucket: bucket, HTTP: http.DefaultClient}, Issuer: issuer, STSEndpoint: sts.url}
	ca, err := builders.LoadCA(context.Background(), d.App, kek)
	if err != nil {
		t.Fatal(err)
	}
	bl, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	builderPort := bl.Addr().(*net.TCPAddr).Port
	hub := &builders.Hub{Pool: d.App, CA: ca, BuiltInToken: builderToken, Version: serverVersion,
		BuilderURL: fmt.Sprintf("https://host.docker.internal:%d", builderPort),
		Images:     map[string]string{"docker": executorImage}, ServerURL: public, BuiltInServerURL: public}
	orch := &runs.Orchestrator{Pool: d.App, KEK: kek, S3: s3, Bucket: bucket, Jobs: js, Hub: hub, NATS: testenv.NATS(t)}
	model := &scriptedModel{}
	ag := &agent.Service{Pool: d.App, KEK: kek, Hub: hub, Model: model, IdleAfter: time.Hour}
	js.OnEnd = orch.JobEnded
	current := new(atomic.Pointer[agent.Service])
	current.Store(ag)
	hub.OnExit = func(jobID string, st *v1.SandboxStatus) {
		if agent.IsSandbox(jobID) {
			current.Load().SandboxExited(jobID, st)
			return
		}
		orch.SandboxExited(jobID, st)
	}
	fake := newFakeGitHub(t)
	gh := &github.GitHub{Pool: d.App, KEK: kek, WebURL: fake.srv.URL, APIURL: fake.srv.URL, PublicURL: public, HTTP: http.DefaultClient,
		Runs: orch, S3: s3, Bucket: bucket}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go orch.Run(ctx)
	go gh.RunWebhooks(ctx, 200*time.Millisecond)
	go gh.RunChecks(ctx, 200*time.Millisecond)
	cfg := server.Config{Pool: d.App, KEK: kek, PublicURL: public, SetupCode: setupCode,
		State: state.Store{S3: s3, Bucket: bucket, KEK: kek}, Jobs: js, Runs: orch, Builders: hub, Binaries: binaries, Issuer: issuer, GitHub: gh, Agent: ag}
	if web != nil {
		cfg.Web = web
	}
	handler := new(atomic.Pointer[http.Handler])
	h := server.New(cfg)
	handler.Store(&h)
	srv.Config.Handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { (*handler.Load()).ServeHTTP(rw, r) })
	bsrv := &http.Server{Handler: server.NewBuilderPort(cfg), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		_ = bsrv.Serve(tls.NewListener(bl, ca.TLSConfig([]string{"127.0.0.1", "host.docker.internal", "localhost"})))
	}()
	t.Cleanup(func() { _ = bsrv.Close() })
	return &world{t: t, srv: srv, owner: d.Owner, s3: s3, bucket: bucket, public: public, jobs: js, runs: orch, hub: hub,
		builderLocal: fmt.Sprintf("https://127.0.0.1:%d", builderPort), sts: sts, github: fake, agent: ag, model: model,
		cfg: cfg, handler: handler, current: current}
}

// restartAgent stands in for a server restart: a new agent service and
// handler on the same database and builders. Recover runs before the new
// handler serves, and every connection drops, as in a restart.
func (w *world) restartAgent(adoptWait time.Duration) {
	w.t.Helper()
	old := w.agent
	ag := &agent.Service{Pool: old.Pool, KEK: old.KEK, Hub: old.Hub, Model: old.Model, IdleAfter: old.IdleAfter, AdoptWait: adoptWait}
	cfg := w.cfg
	cfg.Agent = ag
	h := server.New(cfg)
	if err := ag.Recover(context.Background()); err != nil {
		w.t.Fatal(err)
	}
	w.current.Store(ag)
	w.handler.Store(&h)
	w.srv.CloseClientConnections()
	w.agent = ag
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
	stream    bool
}

func methods() []method {
	var out []method
	for _, f := range []protoreflect.FileDescriptor{
		v1.File_stackwright_v1_auth_proto, v1.File_stackwright_v1_orgs_proto,
		v1.File_stackwright_v1_instance_proto, v1.File_stackwright_v1_meta_proto,
		v1.File_stackwright_v1_projects_proto, v1.File_stackwright_v1_state_proto,
		v1.File_stackwright_v1_executor_proto, v1.File_stackwright_v1_runs_proto, v1.File_stackwright_v1_builder_proto,
		v1.File_stackwright_v1_build_servers_proto, v1.File_stackwright_v1_variables_proto, v1.File_stackwright_v1_cloud_proto,
		v1.File_stackwright_v1_github_proto, v1.File_stackwright_v1_graph_proto, v1.File_stackwright_v1_agent_proto,
	} {
		for i := range f.Services().Len() {
			s := f.Services().Get(i)
			for j := range s.Methods().Len() {
				m := s.Methods().Get(j)
				level := m.Options().(*descriptorpb.MethodOptions).GetIdempotencyLevel()
				out = append(out, method{"/" + string(s.FullName()) + "/" + string(m.Name()),
					level != descriptorpb.MethodOptions_NO_SIDE_EFFECTS, m.IsStreamingClient() || m.IsStreamingServer()})
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
		if m.stream {
			continue
		}
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
		case authz.JobTicket, authz.Job:
			for name, c := range map[string]*client{"anonymous": anonymous, "the owner's session": owner} {
				if _, code := c.call(short(m.procedure), map[string]any{}); code != "unauthenticated" {
					t.Errorf("%s as %s: %s, want unauthenticated", m.procedure, name, code)
				}
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
		if authz.Policy[m.procedure].Access != authz.InOrg || m.stream {
			continue
		}
		if _, code := adminA.call(short(m.procedure), map[string]any{"org": "b"}); code != "not_found" {
			t.Errorf("%s on org b: %s, want not_found", m.procedure, code)
		}
	}
	owner.must("ProjectService/CreateProject", map[string]any{"org": "b", "slug": "secret", "engine": "ENGINE_OPENTOFU",
		"engineVersion": "1.10.7", "layout": "LAYOUT_PLAIN"})
	for _, call := range []struct {
		procedure string
		body      map[string]any
	}{
		{"ProjectService/GetWorkspace", map[string]any{"org": "a", "project": "secret", "workspace": "default"}},
		{"ProjectService/UpdateProject", map[string]any{"org": "a", "project": "secret", "engineVersion": "1.10.7"}},
		{"ProjectService/ArchiveProject", map[string]any{"org": "a", "project": "secret"}},
		{"ProjectService/CreateWorkspace", map[string]any{"org": "a", "project": "secret", "name": "x"}},
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
	step("ProjectService/CreateProject", func() {
		owner.must("ProjectService/CreateProject", map[string]any{"org": "a", "slug": "net", "engine": "ENGINE_OPENTOFU",
			"engineVersion": "1.10.7", "layout": "LAYOUT_CLI_WORKSPACES", "workspaces": []string{"dev", "prod"}})
	})
	step("ProjectService/CreateSampleProject", func() {
		owner.must("ProjectService/CreateSampleProject", map[string]any{"org": "a"})
	})
	step("ProjectService/UpdateProject", func() {
		owner.must("ProjectService/UpdateProject", map[string]any{"org": "a", "project": "net", "path": "network", "engineVersion": "1.10.7"})
	})
	step("ProjectService/CreateWorkspace", func() {
		owner.must("ProjectService/CreateWorkspace", map[string]any{"org": "a", "project": "net", "name": "staging"})
	})
	step("ProjectService/ArchiveWorkspace", func() {
		owner.must("ProjectService/ArchiveWorkspace", map[string]any{"org": "a", "project": "net", "workspace": "staging"})
	})
	step("ProjectService/ArchiveProject", func() {
		owner.must("ProjectService/ArchiveProject", map[string]any{"org": "a", "project": "net"})
	})
	w.project(owner, "a", "web", "prod")
	var tokenID, secret string
	step("TokenService/CreateApiToken", func() {
		out := owner.must("TokenService/CreateApiToken", map[string]any{"org": "a", "name": "ci"})
		tokenID, secret = out["token"].(map[string]any)["id"].(string), out["secret"].(string)
	})
	w.backend(http.MethodPost, "/state/a/web/prod", secret, stateDoc(1, "L1", "a"))
	w.backend(http.MethodPost, "/state/a/web/prod", secret, stateDoc(2, "L1", "a", "b"))
	versions := owner.must("StateService/ListStateVersions", map[string]any{"org": "a", "project": "web", "workspace": "prod"})["versions"].([]any)
	step("StateService/RollbackState", func() {
		owner.must("StateService/RollbackState", map[string]any{"org": "a", "project": "web", "workspace": "prod",
			"id": versions[len(versions)-1].(map[string]any)["id"]})
	})
	w.backend("LOCK", "/state/a/web/prod", secret, lock)
	step("StateService/ForceUnlockState", func() {
		owner.must("StateService/ForceUnlockState", map[string]any{"org": "a", "project": "web", "workspace": "prod"})
	})
	step("TokenService/RevokeApiToken", func() {
		owner.must("TokenService/RevokeApiToken", map[string]any{"org": "a", "id": tokenID})
	})
	step("ProjectService/UpdateWorkspace", func() {
		owner.must("ProjectService/UpdateWorkspace", map[string]any{"org": "a", "project": "web", "workspace": "prod", "autoApply": true})
	})
	ws := map[string]any{"org": "a", "project": "sample", "workspace": "dev"}
	for i, procedure := range []string{"RunService/ApproveRun", "RunService/RejectRun", "RunService/CancelRun"} {
		step("RunService/CreateRun", func() { owner.must("RunService/CreateRun", ws) })
		if _, err := w.owner.Exec(context.Background(), `UPDATE runs SET status = CASE WHEN number = $1 THEN 'needs_approval' ELSE 'canceled' END,
			state_serial = -1`, i+1); err != nil {
			t.Fatal(err)
		}
		step(procedure, func() {
			owner.must(procedure, map[string]any{"org": "a", "project": "sample", "workspace": "dev", "number": i + 1})
		})
	}
	org := map[string]any{"org": "a"}
	var manifest map[string]any
	step("GitHubService/StartAppManifest", func() {
		manifest = owner.must("GitHubService/StartAppManifest", map[string]any{"returnTo": "/a/settings/github"})
	})
	owner.visit("/github/manifest?code=c&state=" + stateOf(t, manifest["url"].(string)))
	step("GitHubService/StartInstall", func() { owner.must("GitHubService/StartInstall", org) })
	w.link(owner)
	step("GitHubService/RefreshRepos", func() { owner.must("GitHubService/RefreshRepos", org) })
	agentSettings := map[string]any{"org": "a", "apiKey": "k", "turnModel": "anthropic/claude-opus-5.5", "shortModel": "anthropic/claude-haiku-5.5", "monthlyCap": 5}
	step("AgentService/SetAgentSettings", func() { owner.must("AgentService/SetAgentSettings", agentSettings) })
	owner.must("ProjectService/CreateProject", map[string]any{"org": "a", "slug": "agent-lab", "engine": "ENGINE_OPENTOFU",
		"engineVersion": "1.10.7", "layout": "LAYOUT_PLAIN", "repoUrl": "https://github.com/acme/infra"})
	lab := map[string]any{"org": "a", "project": "agent-lab", "workspace": "default"}
	// With no build server, each turn ends at once with an error step.
	idle := func(id string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			var status string
			if err := w.owner.QueryRow(context.Background(), `SELECT status FROM agent_sessions WHERE id = $1`, id).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status != "starting" && status != "working" {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the session stays %s", status)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	var sessionID string
	step("AgentService/StartSession", func() {
		sessionID = owner.must("AgentService/StartSession", merge(lab, map[string]any{"message": "hi"}))["session"].(map[string]any)["id"].(string)
	})
	idle(sessionID)
	step("AgentService/SendMessage", func() {
		owner.must("AgentService/SendMessage", map[string]any{"org": "a", "id": sessionID, "message": "again"})
	})
	idle(sessionID)
	step("AgentService/StopTurn", func() { owner.must("AgentService/StopTurn", map[string]any{"org": "a", "id": sessionID}) })
	if _, err := w.owner.Exec(context.Background(), `WITH s AS (UPDATE agent_sessions SET last_seq = last_seq + 1, status = 'waiting_for_approval'
		WHERE id = $1 RETURNING org_id, last_seq) INSERT INTO agent_steps (session_id, org_id, seq, kind, payload) SELECT $1, org_id, last_seq, 'proposal',
		'{"message": "m", "diff": "d"}' FROM s`, sessionID); err != nil {
		t.Fatal(err)
	}
	var proposal int64
	if err := w.owner.QueryRow(context.Background(), `SELECT last_seq FROM agent_sessions WHERE id = $1`, sessionID).Scan(&proposal); err != nil {
		t.Fatal(err)
	}
	step("AgentService/DecideProposal", func() {
		owner.must("AgentService/DecideProposal", map[string]any{"org": "a", "id": sessionID, "proposal": proposal, "approve": false})
	})
	idle(sessionID)
	step("AgentService/CloseSession", func() {
		owner.must("AgentService/CloseSession", map[string]any{"org": "a", "id": sessionID, "discard": true})
	})
	step("GitHubService/UnlinkInstallation", func() {
		owner.must("GitHubService/UnlinkInstallation", map[string]any{"org": "a", "installationId": 7})
	})
	step("GitHubService/DeleteGitHubApp", func() { owner.must("GitHubService/DeleteGitHubApp", map[string]any{}) })
	step("GraphService/SaveGraphLayout", func() {
		owner.must("GraphService/SaveGraphLayout", map[string]any{"org": "a", "project": "web", "workspace": "prod",
			"positions": []map[string]any{{"id": "terraform_data.a", "x": 1, "y": 2}}})
	})
	variable := map[string]any{"org": "a", "scope": map[string]any{"project": "web"}, "category": "VARIABLE_CATEGORY_TERRAFORM", "key": "region"}
	step("VariableService/SetVariable", func() { owner.must("VariableService/SetVariable", merge(variable, map[string]any{"value": "eu"})) })
	step("VariableService/DeleteVariable", func() { owner.must("VariableService/DeleteVariable", variable) })
	step("CloudAccountService/CreateCloudAccount", func() {
		owner.must("CloudAccountService/CreateCloudAccount", map[string]any{"org": "a", "name": "audited", "aws": map[string]any{
			"planRoleArn": "arn:aws:iam::123456789012:role/plan", "applyRoleArn": "arn:aws:iam::123456789012:role/apply", "region": "eu-west-1"}})
	})
	step("CloudAccountService/SetWorkspaceCloudAccount", func() {
		owner.must("CloudAccountService/SetWorkspaceCloudAccount", map[string]any{"org": "a", "project": "web", "workspace": "prod"})
	})
	step("CloudAccountService/DeleteCloudAccount", func() {
		owner.must("CloudAccountService/DeleteCloudAccount", map[string]any{"org": "a", "name": "audited"})
	})
	var cmd string
	step("BuildServerService/CreateJoinCommand", func() {
		cmd = owner.must("BuildServerService/CreateJoinCommand", map[string]any{"org": "a", "name": "audited", "slots": 1})["command"].(string)
	})
	var id *builder.Identity
	step("BuilderService/Enrol", func() {
		var err error
		if id, err = builder.Enrol(context.Background(), builder.PlainClient(w.srv.URL), joinToken.FindStringSubmatch(cmd)[1], 0, nil, "", ""); err != nil {
			t.Fatal(err)
		}
	})
	step("BuildServerService/UpdateBuildServer", func() {
		owner.must("BuildServerService/UpdateBuildServer", map[string]any{"org": "a", "id": id.ServerID, "slots": 3})
	})
	step("BuildServerService/RemoveBuildServer", func() {
		owner.must("BuildServerService/RemoveBuildServer", map[string]any{"org": "a", "id": id.ServerID})
	})
	step("BuildServerService/GetSSHKey", func() { owner.must("BuildServerService/GetSSHKey", org) })
	port := fakeSSHHost(t)
	step("BuildServerService/ScanHostKey", func() {
		owner.must("BuildServerService/ScanHostKey", map[string]any{"org": "a", "host": "127.0.0.1", "port": port})
	})
	step("BuildServerService/EnrolOverSSH", func() {
		if _, _, err := enrolOverSSH(owner, map[string]any{"name": "audited-ssh", "port": port, "hostKey": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"}); err == nil {
			t.Error("enrol with a wrong host key passed")
		}
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
		access := authz.Policy[m.procedure].Access
		if m.write && !covered[m.procedure] && access != authz.JobTicket && access != authz.Job && access != authz.Builder && access != authz.Toolbox {
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

func TestProjects(t *testing.T) {
	w := newWorld(t, nil)
	owner := w.setup()
	create := func(body map[string]any) (map[string]any, string) {
		body["org"] = "a"
		return owner.call("ProjectService/CreateProject", body)
	}

	out, code := create(map[string]any{"slug": "network", "path": "/network/", "engine": "ENGINE_OPENTOFU", "engineVersion": "1.10.7",
		"layout": "LAYOUT_CLI_WORKSPACES", "workspaces": []string{"dev", "staging", "prod"}})
	if code != "" {
		t.Fatalf("create: %s %v", code, out["message"])
	}
	list := owner.must("ProjectService/ListProjects", map[string]any{"org": "a"})["projects"].([]any)
	if len(list) != 1 || len(list[0].(map[string]any)["workspaces"].([]any)) != 3 {
		t.Fatalf("ListProjects = %v, want one project with three workspaces", list)
	}
	if path := list[0].(map[string]any)["path"]; path != "network" {
		t.Errorf("path = %v, want network", path)
	}

	out = owner.must("ProjectService/CreateProject", map[string]any{"org": "a", "slug": "bastion", "engine": "ENGINE_TERRAFORM",
		"engineVersion": "1.5.7", "layout": "LAYOUT_PLAIN", "workspaces": []string{"ignored"}})
	if ws := asJSON(out["project"].(map[string]any)["workspaces"]); !strings.Contains(ws, `"name":"default"`) || strings.Contains(ws, "ignored") {
		t.Errorf("a plain folder got workspaces %s, want default only", ws)
	}
	if _, code := owner.call("ProjectService/CreateWorkspace", map[string]any{"org": "a", "project": "bastion", "name": "dev"}); code != "failed_precondition" {
		t.Errorf("a second workspace in a plain folder: %s", code)
	}

	for _, bad := range []map[string]any{
		{"slug": "tf015", "engine": "ENGINE_TERRAFORM", "engineVersion": "0.15.5", "layout": "LAYOUT_PLAIN"},
		{"slug": "tofu17", "engine": "ENGINE_OPENTOFU", "engineVersion": "1.7.9", "layout": "LAYOUT_PLAIN"},
		{"slug": "noversion", "engine": "ENGINE_OPENTOFU", "engineVersion": "latest", "layout": "LAYOUT_PLAIN"},
		{"slug": "escape", "path": "../etc", "engine": "ENGINE_OPENTOFU", "engineVersion": "1.10.7", "layout": "LAYOUT_PLAIN"},
		{"slug": "Bad Name", "engine": "ENGINE_OPENTOFU", "engineVersion": "1.10.7", "layout": "LAYOUT_PLAIN"},
		{"slug": "nows", "engine": "ENGINE_OPENTOFU", "engineVersion": "1.10.7", "layout": "LAYOUT_CLI_WORKSPACES"},
		{"slug": "twice", "engine": "ENGINE_OPENTOFU", "engineVersion": "1.10.7", "layout": "LAYOUT_CLI_WORKSPACES", "workspaces": []string{"dev", "dev"}},
	} {
		if _, code := create(bad); code != "invalid_argument" {
			t.Errorf("%v: %s, want invalid_argument", bad["slug"], code)
		}
	}
	if _, code := create(map[string]any{"slug": "network", "engine": "ENGINE_OPENTOFU", "engineVersion": "1.10.7", "layout": "LAYOUT_PLAIN"}); code != "already_exists" {
		t.Errorf("a second project named network: %s", code)
	}

	if _, code := owner.call("ProjectService/ArchiveWorkspace", map[string]any{"org": "a", "project": "bastion", "workspace": "default"}); code != "failed_precondition" {
		t.Errorf("archiving the last workspace: %s", code)
	}
	owner.must("ProjectService/ArchiveProject", map[string]any{"org": "a", "project": "network"})
	if list := asJSON(owner.must("ProjectService/ListProjects", map[string]any{"org": "a"})); strings.Contains(list, `"slug":"network"`) {
		t.Error("an archived project is still listed")
	}
	if _, code := owner.call("ProjectService/GetWorkspace", map[string]any{"org": "a", "project": "network", "workspace": "dev"}); code != "not_found" {
		t.Errorf("an archived project's workspace: %s", code)
	}
	if _, code := create(map[string]any{"slug": "network", "engine": "ENGINE_OPENTOFU", "engineVersion": "1.10.7", "layout": "LAYOUT_PLAIN"}); code != "" {
		t.Errorf("reusing an archived project's name: %s", code)
	}

	sample := owner.must("ProjectService/CreateSampleProject", map[string]any{"org": "a"})["project"].(map[string]any)
	again := owner.must("ProjectService/CreateSampleProject", map[string]any{"org": "a"})["project"].(map[string]any)
	if sample["slug"] != "sample" || again["slug"] != "sample-2" || sample["sample"] != true {
		t.Errorf("samples = %v, %v", sample["slug"], again["slug"])
	}
}
