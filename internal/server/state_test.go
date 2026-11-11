package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
)

func (w *world) token(c *client, org string) string {
	w.t.Helper()
	return c.must("TokenService/CreateApiToken", map[string]any{"org": org, "name": "laptop"})["secret"].(string)
}

func (w *world) project(c *client, org, slug string, workspaces ...string) {
	w.t.Helper()
	c.must("ProjectService/CreateProject", map[string]any{"org": org, "slug": slug, "engine": "ENGINE_OPENTOFU",
		"engineVersion": "1.10.7", "layout": "LAYOUT_CLI_WORKSPACES", "workspaces": workspaces})
}

func (w *world) backend(method, path, token, body string) (int, string) {
	w.t.Helper()
	req, _ := http.NewRequest(method, w.srv.URL+path, strings.NewReader(body))
	if token != "" {
		req.SetBasicAuth("stackwright", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func stateDoc(serial int, lineage string, resources ...string) string {
	rs := make([]map[string]string, len(resources))
	for i, r := range resources {
		rs[i] = map[string]string{"type": "terraform_data", "name": r}
	}
	b, _ := json.Marshal(map[string]any{"version": 4, "serial": serial, "lineage": lineage, "resources": rs})
	return string(b)
}

const lock = `{"ID":"lock-1","Operation":"OperationTypeApply","Who":"ci@runner","Version":"1.10.7"}`

func TestStateBackendProtocol(t *testing.T) {
	w := newWorld(t, nil)
	owner := w.setup()
	w.project(owner, "a", "net", "prod")
	token := w.token(owner, "a")
	path := "/state/a/net/prod"

	if code, _ := w.backend(http.MethodGet, path, "", ""); code != http.StatusUnauthorized {
		t.Errorf("no token: %d", code)
	}
	if code, _ := w.backend(http.MethodGet, path, "swt_wrong", ""); code != http.StatusUnauthorized {
		t.Errorf("unknown token: %d", code)
	}
	if code, _ := w.backend(http.MethodGet, path, token, ""); code != http.StatusNoContent {
		t.Errorf("no state yet: %d", code)
	}
	v1 := stateDoc(1, "L1", "a")
	if code, body := w.backend(http.MethodPost, path, token, v1); code != http.StatusOK {
		t.Fatalf("first write: %d %s", code, body)
	}
	if code, body := w.backend(http.MethodGet, path, token, ""); code != http.StatusOK || body != v1 {
		t.Errorf("read back: %d %q", code, body)
	}
	if code, _ := w.backend(http.MethodPost, path, token, v1); code != http.StatusOK {
		t.Errorf("the same serial and bytes again: %d", code)
	}
	if code, _ := w.backend(http.MethodPost, path, token, stateDoc(1, "L1", "a", "b")); code != http.StatusConflict {
		t.Errorf("the same serial with new bytes: %d", code)
	}
	if code, _ := w.backend(http.MethodPost, path, token, stateDoc(5, "other", "a")); code != http.StatusConflict {
		t.Errorf("another lineage: %d", code)
	}
	if code, _ := w.backend(http.MethodPost, path, token, "not json"); code != http.StatusConflict {
		t.Errorf("not JSON: %d", code)
	}

	if code, _ := w.backend("LOCK", path, token, lock); code != http.StatusOK {
		t.Fatalf("lock: %d", code)
	}
	if code, body := w.backend("LOCK", path, token, `{"ID":"lock-2"}`); code != http.StatusLocked || !strings.Contains(body, "ci@runner") {
		t.Errorf("second lock: %d %s", code, body)
	}
	if code, _ := w.backend(http.MethodPost, path, token, stateDoc(2, "L1", "a")); code != http.StatusLocked {
		t.Errorf("a write without the lock ID: %d", code)
	}
	if code, _ := w.backend(http.MethodPost, path+"?ID=lock-1", token, stateDoc(2, "L1", "a")); code != http.StatusOK {
		t.Errorf("a write with the lock ID: %d", code)
	}
	if _, code := owner.call("ProjectService/ArchiveProject", map[string]any{"org": "a", "project": "net"}); code != "failed_precondition" {
		t.Errorf("archiving a locked project: %s", code)
	}
	if code, _ := w.backend("UNLOCK", path, token, `{"ID":"lock-2"}`); code != http.StatusConflict {
		t.Errorf("unlock with the wrong ID: %d", code)
	}
	if code, _ := w.backend("UNLOCK", path, token, lock); code != http.StatusOK {
		t.Errorf("unlock: %d", code)
	}
	if code, _ := w.backend(http.MethodDelete, path, token, ""); code != http.StatusMethodNotAllowed {
		t.Errorf("delete: %d", code)
	}

	var blobKey string
	if err := w.owner.QueryRow(context.Background(), `SELECT blob_key FROM state_versions ORDER BY serial DESC LIMIT 1`).Scan(&blobKey); err != nil {
		t.Fatal(err)
	}
	obj, err := w.s3.GetObject(context.Background(), w.bucket, blobKey, minio.GetObjectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(obj)
	if len(raw) == 0 || bytes.Contains(raw, []byte("terraform_data")) || bytes.Contains(raw, []byte("L1")) {
		t.Errorf("the blob in S3 is not sealed: %q", raw)
	}

	got := owner.must("StateService/GetState", map[string]any{"org": "a", "project": "net", "workspace": "prod"})
	if latest := got["latest"].(map[string]any); latest["serial"] != "2" || latest["resources"] != float64(1) {
		t.Errorf("GetState latest = %v", latest)
	}
	if !strings.Contains(got["backendConfig"].(string), `address        = "`+w.public+path+`"`) {
		t.Errorf("backend config = %q", got["backendConfig"])
	}
}

func TestStateTokensFollowTheUser(t *testing.T) {
	w := newWorld(t, nil)
	owner := w.setup()
	w.project(owner, "a", "net", "prod")
	owner.must("OrgService/CreateOrg", map[string]any{"slug": "b", "name": "B"})
	w.project(owner, "b", "net", "prod")
	viewer := w.member(owner, "a", "viewer@a.test", "ROLE_VIEWER")
	writer := w.member(owner, "a", "writer@a.test", "ROLE_WRITER")
	viewerToken, writerToken, ownerToken := w.token(viewer, "a"), w.token(writer, "a"), w.token(owner, "a")
	doc := stateDoc(1, "L1", "a")

	if code, _ := w.backend(http.MethodGet, "/state/a/net/prod", viewerToken, ""); code != http.StatusNoContent {
		t.Errorf("viewer read: %d", code)
	}
	if code, _ := w.backend(http.MethodPost, "/state/a/net/prod", viewerToken, doc); code != http.StatusForbidden {
		t.Errorf("viewer write: %d", code)
	}
	if code, _ := w.backend("LOCK", "/state/a/net/prod", viewerToken, lock); code != http.StatusForbidden {
		t.Errorf("viewer lock: %d", code)
	}
	if code, _ := w.backend(http.MethodPost, "/state/a/net/prod", writerToken, doc); code != http.StatusOK {
		t.Errorf("writer write: %d", code)
	}
	if code, _ := w.backend(http.MethodGet, "/state/b/net/prod", ownerToken, ""); code != http.StatusNotFound {
		t.Errorf("an org a token in org b: %d", code)
	}
	if code, _ := w.backend(http.MethodGet, "/state/a/net/nope", ownerToken, ""); code != http.StatusNotFound {
		t.Errorf("an unknown workspace: %d", code)
	}

	owner.must("OrgService/SetMemberRole", map[string]any{"org": "a", "userId": w.userID("writer@a.test"), "role": "ROLE_VIEWER"})
	if code, _ := w.backend(http.MethodPost, "/state/a/net/prod", writerToken, stateDoc(2, "L1", "a")); code != http.StatusForbidden {
		t.Errorf("after a demotion: %d", code)
	}
	owner.must("OrgService/RemoveMember", map[string]any{"org": "a", "userId": w.userID("writer@a.test")})
	if code, _ := w.backend(http.MethodGet, "/state/a/net/prod", writerToken, ""); code != http.StatusNotFound {
		t.Errorf("after removal: %d", code)
	}

	tokens := viewer.must("TokenService/ListApiTokens", map[string]any{"org": "a"})["tokens"].([]any)
	viewer.must("TokenService/RevokeApiToken", map[string]any{"org": "a", "id": tokens[0].(map[string]any)["id"]})
	if code, _ := w.backend(http.MethodGet, "/state/a/net/prod", viewerToken, ""); code != http.StatusUnauthorized {
		t.Errorf("a revoked token: %d", code)
	}
	if _, err := w.owner.Exec(context.Background(), `UPDATE api_tokens SET expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if code, _ := w.backend(http.MethodGet, "/state/a/net/prod", ownerToken, ""); code != http.StatusUnauthorized {
		t.Errorf("an expired token: %d", code)
	}
	if _, code := viewer.call("TokenService/RevokeApiToken", map[string]any{"org": "a", "id": tokens[0].(map[string]any)["id"]}); code != "not_found" {
		t.Errorf("revoking twice: %s", code)
	}
}

func TestRollback(t *testing.T) {
	w := newWorld(t, nil)
	owner := w.setup()
	w.project(owner, "a", "net", "prod")
	token := w.token(owner, "a")
	path := "/state/a/net/prod"
	w.backend(http.MethodPost, path, token, stateDoc(1, "L1", "a"))
	w.backend(http.MethodPost, path, token, stateDoc(2, "L1", "a", "b"))

	versions := owner.must("StateService/ListStateVersions", map[string]any{"org": "a", "project": "net", "workspace": "prod"})["versions"].([]any)
	first := versions[len(versions)-1].(map[string]any)
	out := owner.must("StateService/RollbackState", map[string]any{"org": "a", "project": "net", "workspace": "prod", "id": first["id"]})
	if v := out["version"].(map[string]any); v["serial"] != "3" || v["rolledBackFromSerial"] != "1" || v["resources"] != float64(1) {
		t.Errorf("rollback made %v", v)
	}
	_, body := w.backend(http.MethodGet, path, token, "")
	var doc struct {
		Serial  int
		Lineage string
	}
	_ = json.Unmarshal([]byte(body), &doc)
	if doc.Serial != 3 || doc.Lineage != "L1" || strings.Contains(body, `"b"`) {
		t.Errorf("after rollback the state is %s", body)
	}
	if code, _ := w.backend(http.MethodPost, path, token, stateDoc(4, "L1", "a")); code != http.StatusOK {
		t.Errorf("a write after rollback: %d", code)
	}
}

func TestEngines(t *testing.T) {
	if os.Getenv("STACKWRIGHT_TEST_ENGINES") == "" {
		t.Skip("set STACKWRIGHT_TEST_ENGINES=1 to run OpenTofu and Terraform in Docker")
	}
	for _, e := range []struct{ name, image string }{
		{"opentofu", "ghcr.io/opentofu/opentofu:1.10.7"},
		{"terraform", "hashicorp/terraform:1.5.7"},
	} {
		t.Run(e.name, func(t *testing.T) { testEngine(t, e.image) })
	}
}

func testEngine(t *testing.T, image string) {
	w := start(t, nil, true)
	owner := w.setup()
	owner.must("ProjectService/CreateSampleProject", map[string]any{"org": "a"})
	token := w.token(owner, "a")
	setup := owner.must("StateService/GetState", map[string]any{"org": "a", "project": "sample", "workspace": "dev"})

	dir := t.TempDir()
	src, err := os.ReadFile("../../fixtures/sample/main.tf")
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"main.tf": string(src), "dev.tfbackend": setup["backendConfig"].(string)} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) (string, error) {
		args = append([]string{args[0], "-no-color"}, args[1:]...)
		cmd := exec.Command("docker", append([]string{"run", "--rm", "--add-host", "host.docker.internal:host-gateway",
			"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "-e", "HOME=/tmp", "-e", "TF_IN_AUTOMATION=1",
			"-e", "TF_HTTP_PASSWORD=" + token, "-v", dir + ":/work", "-w", "/work", image}, args...)...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	if out, err := run("init", "-input=false", "-backend-config=dev.tfbackend"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if out, err := run("apply", "-input=false", "-auto-approve", "-var", "greeting=one"); err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	versions := owner.must("StateService/ListStateVersions", map[string]any{"org": "a", "project": "sample", "workspace": "dev"})["versions"].([]any)
	if len(versions) == 0 {
		t.Fatal("apply wrote no state version")
	}
	first := versions[len(versions)-1].(map[string]any)

	path := "/state/a/sample/dev"
	if code, _ := w.backend("LOCK", path, token, lock); code != http.StatusOK {
		t.Fatalf("hold the lock: %d", code)
	}
	out, err := run("apply", "-input=false", "-auto-approve", "-lock-timeout=0s", "-var", "greeting=two")
	// Terraform 1.5.7 names the held lock's ID, but prints its own lock info
	// below it; OpenTofu prints the holder's.
	if err == nil || !strings.Contains(out, "already locked: ID=lock-1") ||
		(strings.Contains(image, "opentofu") && !strings.Contains(out, "ci@runner")) {
		t.Errorf("apply while locked: err=%v, want the held lock in:\n%s", err, out)
	}
	w.backend("UNLOCK", path, token, lock)

	if out, err := run("apply", "-input=false", "-auto-approve", "-var", "greeting=two"); err != nil {
		t.Fatalf("second apply: %v\n%s", err, out)
	}
	owner.must("StateService/RollbackState", map[string]any{"org": "a", "project": "sample", "workspace": "dev", "id": first["id"]})
	out, err = run("plan", "-input=false", "-detailed-exitcode", "-var", "greeting=two")
	if code := exitCode(err); code != 2 || !strings.Contains(out, `"one" -> "two"`) {
		t.Errorf("plan after rollback: exit %d, want 2 and the change one -> two in:\n%s", code, out)
	}
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}
