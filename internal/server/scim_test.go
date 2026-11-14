package server_test

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func scimCall(t *testing.T, w *world, token, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, w.srv.URL+"/scim/v2/a"+path, reader)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/scim+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func roleOf(t *testing.T, w *world, email string) string {
	t.Helper()
	var role string
	err := w.owner.QueryRow(t.Context(), `SELECT m.role::text FROM memberships m JOIN users u ON u.id = m.user_id JOIN orgs o ON o.id = m.org_id
		WHERE o.slug = 'a' AND u.email = $1`, email).Scan(&role)
	if err != nil {
		return ""
	}
	return role
}

func TestSCIMRemovingAUserInTheIdentityProviderRemovesTheirAccess(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	owner := w.setup()
	bob := w.member(owner, "a", "bob@a.test", "ROLE_VIEWER")
	org := map[string]any{"org": "a"}

	if code, _ := scimCall(t, w, "sws_wrong", http.MethodGet, "/Users", nil); code != http.StatusUnauthorized {
		t.Errorf("a wrong token: %d", code)
	}
	token := owner.must("SsoService/RotateScimToken", org)["token"].(string)
	owner.must("SsoService/SaveSso", map[string]any{"org": "a", "metadataXml": testIdPMetadata(t), "defaultRole": "ROLE_VIEWER",
		"groupRoles": []any{map[string]any{"group": "deployers", "role": "ROLE_WRITER"}}})

	// The IdP links bob, then a new user, and puts both in a group.
	code, b := scimCall(t, w, token, http.MethodPost, "/Users", map[string]any{"userName": "bob@a.test", "externalId": "okta-bob", "active": true})
	if code != http.StatusCreated || b["userName"] != "bob@a.test" {
		t.Fatalf("create bob: %d %v", code, b)
	}
	if code, _ := scimCall(t, w, token, http.MethodPost, "/Users", map[string]any{"userName": "bob@a.test"}); code != http.StatusConflict {
		t.Errorf("bob again: %d", code)
	}
	_, e := scimCall(t, w, token, http.MethodPost, "/Users", map[string]any{"userName": "erin@a.test", "name": map[string]any{"givenName": "Erin"}})
	if roleOf(t, w, "erin@a.test") != "viewer" {
		t.Errorf("erin joined as %q", roleOf(t, w, "erin@a.test"))
	}
	_, list := scimCall(t, w, token, http.MethodGet, "/Users?filter="+url.QueryEscape(`userName eq "erin@a.test"`), nil)
	if list["totalResults"] != float64(1) {
		t.Errorf("filter: %v", list)
	}
	code, g := scimCall(t, w, token, http.MethodPost, "/Groups", map[string]any{"displayName": "deployers",
		"members": []any{map[string]any{"value": b["id"]}}})
	if code != http.StatusCreated || roleOf(t, w, "bob@a.test") != "writer" {
		t.Fatalf("group: %d %v; bob is %q", code, g, roleOf(t, w, "bob@a.test"))
	}
	scimCall(t, w, token, http.MethodPatch, "/Groups/"+g["id"].(string), map[string]any{"Operations": []any{
		map[string]any{"op": "add", "path": "members", "value": []any{map[string]any{"value": e["id"]}}}}})
	if roleOf(t, w, "erin@a.test") != "writer" {
		t.Errorf("erin in deployers is %q", roleOf(t, w, "erin@a.test"))
	}
	scimCall(t, w, token, http.MethodPatch, "/Groups/"+g["id"].(string), map[string]any{"Operations": []any{
		map[string]any{"op": "remove", "path": `members[value eq "` + e["id"].(string) + `"]`}}})
	if roleOf(t, w, "erin@a.test") != "viewer" {
		t.Errorf("erin out of deployers is %q", roleOf(t, w, "erin@a.test"))
	}

	// Deactivating bob, as Entra ID does with a string, ends his access:
	// the membership, his session and his CLI tokens.
	bob.must("ProjectService/ListProjects", org)
	if _, err := w.owner.Exec(t.Context(), `INSERT INTO cli_tokens (id, user_id, token_hash, name) SELECT gen_random_uuid(), id, 'x', 'laptop'
		FROM users WHERE email = 'bob@a.test'`); err != nil {
		t.Fatal(err)
	}
	code, _ = scimCall(t, w, token, http.MethodPatch, "/Users/"+b["id"].(string), map[string]any{
		"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:PatchOp"}, "Operations": []any{
			map[string]any{"op": "Replace", "path": "active", "value": "False"}}})
	if code != http.StatusOK {
		t.Fatalf("deactivate: %d", code)
	}
	if _, code := bob.call("ProjectService/ListProjects", org); code != "unauthenticated" {
		t.Errorf("bob after the IdP removed him: %q", code)
	}
	if roleOf(t, w, "bob@a.test") != "" {
		t.Errorf("bob is still a member")
	}
	var live int
	if err := w.owner.QueryRow(t.Context(), `SELECT count(*) FROM cli_tokens t JOIN users u ON u.id = t.user_id
		WHERE u.email = 'bob@a.test' AND t.revoked_at IS NULL`).Scan(&live); err != nil || live != 0 {
		t.Errorf("bob's CLI tokens: %d %v", live, err)
	}

	// Deleting erin removes her too; an owner is left as is.
	if code, _ := scimCall(t, w, token, http.MethodDelete, "/Users/"+e["id"].(string), nil); code != http.StatusNoContent {
		t.Errorf("delete erin: %d", code)
	}
	if roleOf(t, w, "erin@a.test") != "" {
		t.Errorf("erin is still a member")
	}
	_, o := scimCall(t, w, token, http.MethodPost, "/Users", map[string]any{"userName": "owner@a.test"})
	scimCall(t, w, token, http.MethodPatch, "/Users/"+o["id"].(string), map[string]any{"Operations": []any{
		map[string]any{"op": "replace", "value": map[string]any{"active": false}}}})
	if roleOf(t, w, "owner@a.test") != "owner" {
		t.Errorf("SCIM changed the owner to %q", roleOf(t, w, "owner@a.test"))
	}
	owner.must("ProjectService/ListProjects", org)
}

// testIdPMetadata is an IdP's metadata with a new self-signed certificate.
func testIdPMetadata(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "idp.test"}, NotBefore: time.Now(),
		NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return `<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" entityID="https://idp.test/metadata">
  <IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">
    <KeyDescriptor use="signing"><KeyInfo xmlns="http://www.w3.org/2000/09/xmldsig#"><X509Data><X509Certificate>` +
		base64.StdEncoding.EncodeToString(der) + `</X509Certificate></X509Data></KeyInfo></KeyDescriptor>
    <SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="https://idp.test/sso"/>
  </IDPSSODescriptor>
</EntityDescriptor>`
}
