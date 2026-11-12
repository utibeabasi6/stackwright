package server_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"maps"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub answers the parts of github.com and its API that Stackwright
// calls. Its web and API share one address.
type fakeGitHub struct {
	t   *testing.T
	srv *httptest.Server

	mu            sync.Mutex
	key           *rsa.PrivateKey
	webhookSecret string
	installations map[int64]string
	// visible lists the installations that the user token can see.
	visible []int64
	repos   []fakeRepo
	commits map[string]map[string]string
	// heads holds each branch's newest commit.
	heads        map[string]string
	compare      map[string][]string
	checks       map[int64]*fakeCheck
	nextID       int64
	comments     map[int64]string
	commentPosts int
	// gitRoot holds bare repositories, served with git http-backend.
	gitRoot string
	git     http.Handler
}

type fakeRepo struct {
	ID       int64
	FullName string
}

type fakeCheck struct {
	ID          int64
	Name        string
	HeadSHA     string
	Status      string
	Conclusion  string
	Title       string
	Annotations []map[string]any
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeGitHub{t: t, key: key, webhookSecret: "webhook-secret-" + rand.Text()[:8],
		installations: map[int64]string{}, commits: map[string]map[string]string{}, heads: map[string]string{}, compare: map[string][]string{},
		checks: map[int64]*fakeCheck{}, comments: map[int64]string{}, nextID: 100}
	f.gitRoot = t.TempDir()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	f.git = &cgi.Handler{Path: gitPath, Args: []string{"http-backend"}, Env: []string{
		"GIT_PROJECT_ROOT=" + f.gitRoot, "GIT_HTTP_EXPORT_ALL=1",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.receivepack", "GIT_CONFIG_VALUE_0=true",
	}}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

// gitRepo makes a bare repository for fullName whose main branch has one
// commit with files.
func (f *fakeGitHub) gitRepo(fullName string, files map[string]string) {
	f.t.Helper()
	work := f.t.TempDir()
	for name, body := range files {
		path := filepath.Join(work, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			f.t.Fatal(err)
		}
	}
	bare := filepath.Join(f.gitRoot, fullName+".git")
	for _, args := range [][]string{
		{"init", "--bare", "--initial-branch=main", bare},
		{"-C", work, "init", "--initial-branch=main"},
		{"-C", work, "add", "--all"},
		{"-C", work, "-c", "user.name=Test", "-c", "user.email=test@a.test", "commit", "-m", "Start"},
		{"-C", work, "push", bare, "main"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			f.t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

// refs lists a repository's branches and their commits.
func (f *fakeGitHub) refs(fullName string) map[string]string {
	f.t.Helper()
	out, err := exec.Command("git", "--git-dir", filepath.Join(f.gitRoot, fullName+".git"), "for-each-ref", "--format=%(refname) %(objectname)").Output()
	if err != nil {
		f.t.Fatal(err)
	}
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if name, sha, ok := strings.Cut(line, " "); ok {
			refs[name] = sha
		}
	}
	return refs
}

// show returns a file at a revision of a repository.
func (f *fakeGitHub) show(fullName, rev, path string) string {
	f.t.Helper()
	out, err := exec.Command("git", "--git-dir", filepath.Join(f.gitRoot, fullName+".git"), "show", rev+":"+path).CombinedOutput()
	if err != nil {
		f.t.Fatalf("git show %s:%s: %v: %s", rev, path, err, out)
	}
	return string(out)
}

// commit records a commit's files, for its tarball.
func (f *fakeGitHub) commit(sha string, files map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commits[sha] = files
}

func (f *fakeGitHub) diff(base, head string, files ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.compare[base+"..."+head] = files
}

func (f *fakeGitHub) allChecks() []fakeCheck {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCheck
	for _, c := range f.checks {
		out = append(out, *c)
	}
	return out
}

func (f *fakeGitHub) json(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// appJWT checks the App's token as GitHub does: RS256 with the App's key.
func (f *fakeGitHub) appJWT(r *http.Request) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(tok, ".")
	if !ok || len(parts) != 3 {
		return false
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	return rsa.VerifyPKCS1v15(&f.key.PublicKey, crypto.SHA256, digest[:], sig) == nil
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Path, ".git/") {
		// Only the Git proxy reaches here, with the installation token.
		if user, pass, ok := r.BasicAuth(); !ok || user != "x-access-token" || !strings.HasPrefix(pass, "ghs_") {
			w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
			http.Error(w, "an installation token is required", http.StatusUnauthorized)
			return
		}
		f.git.ServeHTTP(w, r)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(p, "/app-manifests/"):
		der := x509.MarshalPKCS1PrivateKey(f.key)
		f.json(w, map[string]any{"id": 4242, "slug": "stackwright-test", "name": "Stackwright test", "html_url": f.srv.URL + "/apps/stackwright-test",
			"owner": map[string]string{"login": "acme"}, "client_id": "Iv1.client", "client_secret": "client-secret",
			"webhook_secret": f.webhookSecret, "pem": string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}))})
	case r.Method == http.MethodPost && strings.HasPrefix(p, "/app/installations/"):
		if !f.appJWT(r) {
			http.Error(w, "bad JWT", http.StatusUnauthorized)
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/app/installations/"), "/access_tokens")
		f.json(w, map[string]any{"token": "ghs_" + id, "expires_at": time.Now().Add(time.Hour)})
	case r.Method == http.MethodPost && p == "/login/oauth/access_token":
		var in map[string]string
		_ = json.Unmarshal(body, &in)
		if in["code"] != "good-code" || in["client_secret"] != "client-secret" {
			f.json(w, map[string]string{"error": "bad_verification_code", "error_description": "The code is wrong."})
			return
		}
		f.json(w, map[string]string{"access_token": "ghu_user"})
	case p == "/user/installations":
		var list []map[string]any
		for _, id := range f.visible {
			list = append(list, map[string]any{"id": id, "account": map[string]string{"login": f.installations[id]}})
		}
		f.json(w, map[string]any{"installations": list})
	case p == "/installation/repositories":
		var list []map[string]any
		for _, repo := range f.repos {
			list = append(list, map[string]any{"id": repo.ID, "full_name": repo.FullName, "default_branch": "main", "private": true})
		}
		f.json(w, map[string]any{"repositories": list})
	case strings.Contains(p, "/commits/"):
		sha, ok := f.heads[p[strings.LastIndex(p, "/")+1:]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		f.json(w, map[string]string{"sha": sha})
	case strings.Contains(p, "/compare/"):
		spec := p[strings.Index(p, "/compare/")+len("/compare/"):]
		files, ok := f.compare[spec]
		if !ok {
			http.NotFound(w, r)
			return
		}
		var list []map[string]string
		for _, name := range files {
			list = append(list, map[string]string{"filename": name})
		}
		f.json(w, map[string]any{"files": list})
	case strings.Contains(p, "/tarball/"):
		sha := p[strings.LastIndex(p, "/")+1:]
		files, ok := f.commits[sha]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(githubTarball("acme-infra-"+sha[:7], sha, files))
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/check-runs"):
		f.nextID++
		c := f.check(f.nextID, body)
		f.checks[c.ID] = &c
		f.json(w, map[string]any{"id": c.ID})
	case r.Method == http.MethodPatch && strings.Contains(p, "/check-runs/"):
		id, _ := strconv.ParseInt(p[strings.LastIndex(p, "/")+1:], 10, 64)
		old, ok := f.checks[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		c := f.check(id, body)
		c.HeadSHA = old.HeadSHA
		f.checks[id] = &c
		f.json(w, map[string]any{"id": id})
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/comments"):
		f.nextID++
		var in map[string]string
		_ = json.Unmarshal(body, &in)
		f.comments[f.nextID] = in["body"]
		f.commentPosts++
		f.json(w, map[string]any{"id": f.nextID})
	case r.Method == http.MethodPatch && strings.Contains(p, "/issues/comments/"):
		id, _ := strconv.ParseInt(p[strings.LastIndex(p, "/")+1:], 10, 64)
		var in map[string]string
		_ = json.Unmarshal(body, &in)
		f.comments[id] = in["body"]
		f.json(w, map[string]any{"id": id})
	default:
		f.t.Logf("fake GitHub: no route for %s %s", r.Method, p)
		http.NotFound(w, r)
	}
}

func (f *fakeGitHub) check(id int64, body []byte) fakeCheck {
	var in struct {
		Name       string `json:"name"`
		HeadSHA    string `json:"head_sha"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		Output     struct {
			Title       string           `json:"title"`
			Annotations []map[string]any `json:"annotations"`
		} `json:"output"`
	}
	_ = json.Unmarshal(body, &in)
	return fakeCheck{ID: id, Name: in.Name, HeadSHA: in.HeadSHA, Status: in.Status, Conclusion: in.Conclusion, Title: in.Output.Title,
		Annotations: in.Output.Annotations}
}

// githubTarball builds an archive as GitHub does: a pax global header with
// the commit, and every file under one top folder.
func githubTarball(top, sha string, files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": sha}})
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: top + "/", Mode: 0o755})
	for _, name := range slices.Sorted(maps.Keys(files)) {
		_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: top + "/" + name, Mode: 0o644, Size: int64(len(files[name]))})
		_, _ = tw.Write([]byte(files[name]))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// deliver posts a webhook signed with the App's secret, as GitHub does.
func (f *fakeGitHub) deliver(w *world, event, delivery string, payload any) int {
	w.t.Helper()
	body, _ := json.Marshal(payload)
	mac := hmac.New(sha256.New, []byte(f.webhookSecret))
	mac.Write(body)
	req, _ := http.NewRequest(http.MethodPost, w.srv.URL+"/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}
