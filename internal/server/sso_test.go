package server_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

// keycloakImage is Keycloak 26.8.1, a real SAML identity provider.
const keycloakImage = "quay.io/keycloak/keycloak@sha256:3f3c5f8e03c614774b4bd9173b88391fefa4153bfc010d8012d59ad825377332"

type keycloak struct {
	t     *testing.T
	base  string
	token string
}

func startKeycloak(t *testing.T) *keycloak {
	t.Helper()
	out, err := exec.Command("docker", "run", "-d", "--quiet", "-p", "127.0.0.1::8080", "-e", "KC_BOOTSTRAP_ADMIN_USERNAME=admin",
		"-e", "KC_BOOTSTRAP_ADMIN_PASSWORD=admin", keycloakImage, "start-dev").CombinedOutput()
	if err != nil {
		t.Fatalf("start Keycloak: %v %s", err, out)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })
	port, err := exec.Command("docker", "port", id, "8080").CombinedOutput()
	if err != nil {
		logs, _ := exec.Command("docker", "logs", id).CombinedOutput()
		t.Fatalf("the Keycloak port: %v %s\n%s", err, port, logs)
	}
	k := &keycloak{t: t, base: "http://" + strings.TrimSpace(strings.Split(string(port), "\n")[0])}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		resp, err := http.PostForm(k.base+"/realms/master/protocol/openid-connect/token",
			url.Values{"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {"admin"}, "password": {"admin"}})
		if err == nil {
			var tok struct {
				AccessToken string `json:"access_token"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&tok)
			_ = resp.Body.Close()
			if tok.AccessToken != "" {
				k.token = tok.AccessToken
				return k
			}
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", id).CombinedOutput()
			t.Fatalf("Keycloak did not start: %v\n%s", err, logs)
		}
		time.Sleep(2 * time.Second)
	}
}

func (k *keycloak) admin(method, path string, body any) {
	k.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, k.base+"/admin/realms"+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+k.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		k.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(resp.Body)
		k.t.Fatalf("Keycloak %s %s: %s %s", method, path, resp.Status, msg)
	}
}

// realm sets up realm acme with a SAML client for an org's service
// provider, a group, and users.
func (k *keycloak) realm(spEntityID, acs string) {
	k.admin(http.MethodPost, "", map[string]any{"realm": "acme", "enabled": true})
	k.admin(http.MethodPost, "/acme/groups", map[string]any{"name": "platform-admins"})
	k.admin(http.MethodPost, "/acme/clients", map[string]any{
		"clientId": spEntityID, "protocol": "saml", "enabled": true, "redirectUris": []string{acs}, "frontchannelLogout": false,
		"attributes": map[string]string{
			"saml.assertion.signature": "true", "saml.server.signature": "true", "saml.client.signature": "false",
			"saml_name_id_format": "email", "saml.force.name.id.format": "true", "saml.authnstatement": "true",
			"saml_assertion_consumer_url_post": acs, "saml.signature.algorithm": "RSA_SHA256",
		},
		"protocolMappers": []map[string]any{{"name": "groups", "protocol": "saml", "protocolMapper": "saml-group-membership-mapper",
			"config": map[string]string{"attribute.name": "groups", "full.path": "false", "single": "false", "attribute.nameformat": "Basic"}}},
	})
	for _, u := range []struct {
		name, email string
		groups      []string
	}{{"alice", "alice@acme.test", []string{"/platform-admins"}}, {"dave", "dave@acme.test", nil}, {"carol", "carol@elsewhere.test", nil}} {
		k.admin(http.MethodPost, "/acme/users", map[string]any{"username": u.name, "email": u.email, "emailVerified": true, "enabled": true,
			"firstName": strings.ToUpper(u.name[:1]) + u.name[1:], "lastName": "Test", "groups": u.groups,
			"credentials": []map[string]any{{"type": "password", "value": "pw-" + u.name + "-1234", "temporary": false}}})
	}
}

var (
	loginAction = regexp.MustCompile(`<form[^>]*id="kc-form-login"[^>]*action="([^"]+)"`)
	postAction  = regexp.MustCompile(`<form[^>]*action="([^"]+)"`)
	hiddenInput = regexp.MustCompile(`<input[^>]*name="(SAMLResponse|RelayState)"[^>]*value="([^"]*)"`)
)

// samlSignIn signs a user in through the IdP as a browser does, and
// returns a client with the session and the response it posted.
func samlSignIn(t *testing.T, w *world, user string) (*client, url.Values, string) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar}
	noFollow := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(w.srv.URL + "/auth/saml/a/start?next=/a")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	idp := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(idp, "SAMLRequest=") {
		t.Fatalf("start: %d %s", resp.StatusCode, idp)
	}
	resp, err = browser.Get(idp)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	m := loginAction.FindSubmatch(page)
	if m == nil {
		t.Fatalf("no login form:\n%s", page)
	}
	resp, err = browser.PostForm(html.UnescapeString(string(m[1])), url.Values{"username": {user}, "password": {"pw-" + user + "-1234"}})
	if err != nil {
		t.Fatal(err)
	}
	page, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	form := url.Values{}
	for _, h := range hiddenInput.FindAllSubmatch(page, -1) {
		form.Set(string(h[1]), html.UnescapeString(string(h[2])))
	}
	action := postAction.FindSubmatch(page)
	if form.Get("SAMLResponse") == "" || action == nil {
		t.Fatalf("no SAML response:\n%s", page)
	}
	c := w.client()
	c.http.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err = c.http.PostForm(html.UnescapeString(string(action[1])), form)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	c.http.CheckRedirect = nil
	return c, form, resp.Header.Get("Location")
}

func TestSAMLSignsInThroughTheOrgsIdentityProvider(t *testing.T) {
	t.Parallel()
	requireDocker(t)
	w := newWorld(t, nil)
	owner := w.setup()
	bob := w.member(owner, "a", "bob@a.test", "ROLE_WRITER")
	k := startKeycloak(t)
	sso := owner.must("SsoService/GetSso", map[string]any{"org": "a"})
	if sso["acsUrl"] != w.srv.URL+"/auth/saml/a/acs" || sso["spEntityId"] != w.srv.URL+"/auth/saml/a/metadata" {
		t.Fatalf("the service provider: %v", sso)
	}
	k.realm(sso["spEntityId"].(string), sso["acsUrl"].(string))
	if _, code := owner.call("SsoService/SaveSso", map[string]any{"org": "a", "metadataUrl": k.base + "/realms/acme/protocol/saml/descriptor",
		"defaultRole": "ROLE_OWNER"}); code != "invalid_argument" {
		t.Errorf("an IdP that makes owners: %q", code)
	}
	if _, code := bob.call("SsoService/GetSso", map[string]any{"org": "a"}); code != "permission_denied" {
		t.Errorf("a writer read the SSO settings: %q", code)
	}
	saved := owner.must("SsoService/SaveSso", map[string]any{"org": "a", "metadataUrl": k.base + "/realms/acme/protocol/saml/descriptor",
		"defaultRole": "ROLE_VIEWER", "groupRoles": []any{map[string]any{"group": "platform-admins", "role": "ROLE_ADMIN"}}})["sso"].(map[string]any)
	if s := saved["sso"].(map[string]any); s["idpEntityId"] != k.base+"/realms/acme" || len(asList(s["certificatesExpire"])) == 0 {
		t.Fatalf("saved: %v", saved)
	}

	// A user the IdP knows joins with the role of their group.
	alice, posted, next := samlSignIn(t, w, "alice")
	if next != "/a" {
		t.Fatalf("alice's sign-in went to %q", next)
	}
	me := alice.must("AuthService/GetMe", map[string]any{})
	if orgs := asList(me["orgs"]); me["user"].(map[string]any)["email"] != "alice@acme.test" || len(orgs) != 1 ||
		orgs[0].(map[string]any)["role"] != "ROLE_ADMIN" {
		t.Errorf("alice: %v", me)
	}
	if _, next := samlPost(t, w, posted); next != "/sign-in?error=saml-expired" {
		t.Errorf("a replayed response went to %q", next)
	}
	tampered := url.Values{"RelayState": posted["RelayState"]}
	raw, _ := base64.StdEncoding.DecodeString(posted.Get("SAMLResponse"))
	tampered.Set("SAMLResponse", base64.StdEncoding.EncodeToString(bytes.ReplaceAll(raw, []byte("alice@acme.test"), []byte("owner@a.test"))))
	if _, next := samlPost(t, w, tampered); next != "/sign-in?error=saml-response" {
		t.Errorf("a changed response went to %q", next)
	}
	if _, _, next := samlSignIn(t, w, "dave"); next != "/a" {
		t.Errorf("dave's sign-in went to %q", next)
	}
	if _, err := w.owner.Exec(t.Context(), `INSERT INTO users (id, email) VALUES (gen_random_uuid(), 'carol@elsewhere.test')`); err != nil {
		t.Fatal(err)
	}
	if _, _, next := samlSignIn(t, w, "carol"); next != "/sign-in?error=saml-account" {
		t.Errorf("an account outside the org went to %q", next)
	}

	// With SSO required, a member signed in another way is turned away,
	// but an owner is not; a SAML session opens only its own org.
	owner.must("SsoService/SaveSso", map[string]any{"org": "a", "metadataUrl": k.base + "/realms/acme/protocol/saml/descriptor",
		"defaultRole": "ROLE_VIEWER", "required": true, "groupRoles": []any{map[string]any{"group": "platform-admins", "role": "ROLE_ADMIN"}}})
	org := map[string]any{"org": "a"}
	if _, code := bob.call("ProjectService/ListProjects", org); code != "permission_denied" {
		t.Errorf("bob with a password session: %q", code)
	}
	if orgs := asList(bob.must("AuthService/GetMe", map[string]any{})["orgs"]); orgs[0].(map[string]any)["needsSso"] != true {
		t.Errorf("bob's orgs: %v", orgs)
	}
	owner.must("ProjectService/ListProjects", org)
	alice.must("ProjectService/ListProjects", org)
	owner.must("OrgService/CreateOrg", map[string]any{"slug": "b", "name": "B"})
	if _, err := w.owner.Exec(t.Context(), `INSERT INTO memberships (org_id, user_id, role) SELECT o.id, u.id, 'viewer' FROM orgs o, users u
		WHERE o.slug = 'b' AND u.email = 'alice@acme.test'`); err != nil {
		t.Fatal(err)
	}
	if _, code := alice.call("ProjectService/ListProjects", map[string]any{"org": "b"}); code != "permission_denied" {
		t.Errorf("alice's SAML session in another org: %q", code)
	}
	if orgs := asList(alice.must("AuthService/GetMe", map[string]any{})["orgs"]); len(orgs) != 1 {
		t.Errorf("alice's SAML session lists %v", orgs)
	}
}

func samlPost(t *testing.T, w *world, form url.Values) (int, string) {
	t.Helper()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.PostForm(w.srv.URL+"/auth/saml/a/acs", form)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Location")
}
