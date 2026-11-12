package server_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/crypto/ssh"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/gen/stackwright/v1/stackwrightv1connect"
	"github.com/stackwright/stackwright/internal/builder"
	"github.com/stackwright/stackwright/internal/builders"
	"github.com/stackwright/stackwright/internal/sandbox"
)

var (
	joinToken = regexp.MustCompile(`echo '(swe_[A-Za-z0-9_-]+)'`)
	joinPin   = regexp.MustCompile(`--ca-sha256 ([0-9a-f]{64})`)
)

// join enrols a build server into org "a" with a join command, the way a
// user's machine would, and returns its identity.
func (w *world) join(owner *client, name string, labels map[string]string) *builder.Identity {
	w.t.Helper()
	cmd := owner.must("BuildServerService/CreateJoinCommand", map[string]any{"org": "a", "name": name, "labels": labels, "slots": 1})["command"].(string)
	token, pin := joinToken.FindStringSubmatch(cmd), joinPin.FindStringSubmatch(cmd)
	if token == nil || pin == nil {
		w.t.Fatalf("join command lacks a token or a pin:\n%s", cmd)
	}
	client, err := builder.PinnedClient(w.builderLocal, pin[1])
	if err != nil {
		w.t.Fatal(err)
	}
	id, err := builder.Enrol(context.Background(), client, token[1], 0, nil, "", w.builderLocal)
	if err != nil {
		w.t.Fatal(err)
	}
	return id
}

func (w *world) servers(c *client) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, s := range asList(c.must("BuildServerService/ListBuildServers", map[string]any{"org": "a"})["buildServers"]) {
		m := s.(map[string]any)
		out[m["name"].(string)] = m
	}
	return out
}

// hello opens a stream with id and sends a hello for version.
func hello(id *builder.Identity, version string) *connect.BidiStreamForClient[v1.ConnectRequest, v1.ConnectResponse] {
	stream := id.Client().Connect(context.Background())
	_ = stream.Send(&v1.ConnectRequest{Body: &v1.ConnectRequest_Hello{Hello: &v1.BuilderHello{
		Platform: "linux_arm64", Version: version, Backend: "docker",
	}}})
	return stream
}

func TestBuilderEnrolNeedsAToken(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	owner := w.setup()
	if _, err := builder.Enrol(context.Background(), builder.PlainClient(w.srv.URL), "swe_wrong", 0, nil, "", ""); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("wrong token: %v", err)
	}
	cmd := owner.must("BuildServerService/CreateJoinCommand", map[string]any{"org": "a", "name": "once", "slots": 1})["command"].(string)
	token := joinToken.FindStringSubmatch(cmd)[1]
	if _, err := builder.Enrol(context.Background(), builder.PlainClient(w.srv.URL), token, 0, nil, "", ""); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if _, err := builder.Enrol(context.Background(), builder.PlainClient(w.srv.URL), token, 0, nil, "", ""); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("second use: %v", err)
	}
	if _, code := owner.call("BuildServerService/CreateJoinCommand", map[string]any{"org": "a", "name": "once", "slots": 1}); code != "already_exists" {
		t.Errorf("a second server named once: %q", code)
	}
}

func TestBuilderConnectNeedsACertificate(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	var protocols http.Protocols
	protocols.SetHTTP2(true)
	protocols.SetUnencryptedHTTP2(true)
	plain := stackwrightv1connect.NewBuilderServiceClient(&http.Client{Transport: &http.Transport{Protocols: &protocols}}, w.srv.URL)

	roots := x509.NewCertPool()
	roots.AddCert(mustParse(t, w.hub.CA.Certificate()))
	noCert := stackwrightv1connect.NewBuilderServiceClient(&http.Client{Transport: &http.Transport{
		Protocols: &protocols, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots},
	}}, w.builderLocal)

	for name, c := range map[string]stackwrightv1connect.BuilderServiceClient{"server port": plain, "builder port": noCert} {
		stream := c.Connect(context.Background())
		_ = stream.Send(&v1.ConnectRequest{Body: &v1.ConnectRequest_Hello{Hello: &v1.BuilderHello{Platform: "linux_arm64", Backend: "docker"}}})
		if _, err := stream.Receive(); connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Errorf("%s without a certificate: %v", name, err)
		}
	}
}

func mustParse(t *testing.T, der []byte) *x509.Certificate {
	t.Helper()
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestBuildServerRemovedIsRefusedAtOnce(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	owner := w.setup()
	id := w.join(owner, "gone", nil)
	stream := hello(id, "")
	wait, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.hub.WaitOnline(wait, id.ServerID); err != nil {
		t.Fatal(err)
	}
	if s := w.servers(owner)["gone"]; s["status"] != "STATUS_ONLINE" {
		t.Fatalf("before removal: %v", s)
	}

	owner.must("BuildServerService/RemoveBuildServer", map[string]any{"org": "a", "id": id.ServerID})
	closed := make(chan error, 1)
	go func() {
		for {
			if _, err := stream.Receive(); err != nil {
				closed <- err
				return
			}
		}
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Error("the stream stayed open after removal")
	}
	if _, ok := w.hub.Status()[id.ServerID]; ok {
		t.Error("the hub still holds the removed server")
	}
	if _, ok := w.servers(owner)["gone"]; ok {
		t.Error("the removed server is still listed")
	}
	if _, err := hello(id, "").Receive(); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("reconnect after removal: %v", err)
	}
}

func TestBuildServerVersions(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	owner := w.setup()
	old := w.join(owner, "old", nil)
	_ = hello(old, "v0.4.2")
	wait, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.hub.WaitOnline(wait, old.ServerID); err != nil {
		t.Fatalf("two minor versions behind: %v", err)
	}
	s := w.servers(owner)["old"]
	if s["status"] != "STATUS_ONLINE" || !strings.Contains(fmt.Sprint(s["upgrade"]), "older than the server") {
		t.Errorf("two minor versions behind: %v", s)
	}

	older := w.join(owner, "older", nil)
	if _, err := hello(older, "v0.3.9").Receive(); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("three minor versions behind: %v", err)
	}
	if _, err := hello(w.join(owner, "major", nil), "v1.6.0").Receive(); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("another major version: %v", err)
	}
}

func TestBuildServerRenewsItsCertificate(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	owner := w.setup()
	id := w.join(owner, "renew", nil)
	stream := hello(id, "")
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&v1.ConnectRequest{Body: &v1.ConnectRequest_Renew{Renew: &v1.RenewCertificate{Csr: csr}}}); err != nil {
		t.Fatal(err)
	}
	msg, err := stream.Receive()
	if err != nil {
		t.Fatal(err)
	}
	cert := mustParse(t, msg.GetCertificate())
	if err := cert.CheckSignatureFrom(mustParse(t, w.hub.CA.Certificate())); err != nil || cert.Subject.CommonName != id.ServerID {
		t.Errorf("renewed certificate: %v, %q", err, cert.Subject.CommonName)
	}
	if left := time.Until(cert.NotAfter); left < builders.CertLifetime-time.Minute || left > builders.CertLifetime {
		t.Errorf("renewed certificate lasts %v", left)
	}
	var n int
	if err := w.owner.QueryRow(context.Background(), `SELECT count(*) FROM build_server_certs WHERE build_server_id = $1`, id.ServerID).Scan(&n); err != nil || n != 2 {
		t.Errorf("recorded certificates: %d, %v", n, err)
	}
}

func TestBuildServerLabelsAndDraining(t *testing.T) {
	t.Parallel()
	requireDocker(t)
	w := start(t, nil, true)
	owner := w.setup()
	owner.must("ProjectService/CreateSampleProject", map[string]any{"org": "a"})
	rw := &runWorld{world: w, owner: owner}
	eu := w.join(owner, "eu", map[string]string{"region": "eu"})
	w.runBuilder(eu)
	w.runBuilder(w.join(owner, "us", map[string]string{"region": "us"}))
	owner.must("ProjectService/UpdateWorkspace", dev(map[string]any{"requiredLabels": map[string]any{"values": map[string]string{"region": "eu"}}}))

	owner.must("RunService/CreateRun", dev(map[string]any{"planOnly": true}))
	deadline := time.Now().Add(4 * time.Minute)
	for {
		run := owner.must("RunService/GetRun", dev(map[string]any{"number": 1}))["run"].(map[string]any)
		if run["buildServer"] != nil {
			if run["buildServer"] != "eu" {
				t.Fatalf("run 1 went to %v", run["buildServer"])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run 1 was never placed: %v", run)
		}
		time.Sleep(100 * time.Millisecond)
	}
	owner.must("BuildServerService/UpdateBuildServer", map[string]any{"org": "a", "id": eu.ServerID, "draining": true})
	if run := rw.waitRun(1, "RUN_STATUS_PLANNED", "RUN_STATUS_FAILED"); run["status"] != "RUN_STATUS_PLANNED" {
		t.Fatalf("run 1 on a draining server: %v", run)
	}
	if s := w.servers(owner)["eu"]; s["status"] != "STATUS_DRAINING" {
		t.Errorf("eu: %v", s)
	}

	owner.must("RunService/CreateRun", dev(map[string]any{"planOnly": true}))
	time.Sleep(3 * time.Second)
	run := owner.must("RunService/GetRun", dev(map[string]any{"number": 2}))["run"].(map[string]any)
	if run["buildServer"] != nil {
		t.Fatalf("run 2 went to %v while eu drains", run["buildServer"])
	}
	var waiting bool
	if err := w.owner.QueryRow(context.Background(), `SELECT EXISTS (SELECT FROM run_events WHERE run_id = $1 AND type = 'run.waiting')`,
		run["id"]).Scan(&waiting); err != nil || !waiting {
		t.Errorf("run 2 has no run.waiting event: %v", err)
	}

	owner.must("BuildServerService/UpdateBuildServer", map[string]any{"org": "a", "id": eu.ServerID, "draining": false})
	if run := rw.waitRun(2, "RUN_STATUS_PLANNED", "RUN_STATUS_FAILED"); run["status"] != "RUN_STATUS_PLANNED" || run["buildServer"] != "eu" {
		t.Errorf("run 2 after eu resumed: %v", run)
	}
}

// sshHost runs the test image from testdata/sshd with the host's Docker
// socket, and returns its container id and SSH port.
func sshHost(t *testing.T, authorizedKey, cacheVolume string, noDocker bool) (string, int) {
	t.Helper()
	if out, err := exec.Command("docker", "build", "-q", "-t", "stackwright/test-sshd:dev", "testdata/sshd").CombinedOutput(); err != nil {
		t.Fatalf("build the SSH host image: %v: %s", err, out)
	}
	args := []string{"run", "-d", "--rm", "-p", "127.0.0.1::22", "-e", "AUTHORIZED_KEY=" + authorizedKey,
		"-v", "/var/run/docker.sock:/var/run/docker.sock", "-v", cacheVolume + ":/var/cache/stackwright-builder", "-e", "SANDBOX_CACHE=" + cacheVolume}
	if noDocker {
		args = append(args, "-e", "NO_DOCKER=1")
	}
	out, err := exec.Command("docker", append(args, "stackwright/test-sshd:dev")...).CombinedOutput()
	if err != nil {
		t.Fatalf("start the SSH host: %v: %s", err, out)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := exec.Command("docker", "exec", id, "cat", "/var/log/stackwright-builder.log").CombinedOutput()
			t.Logf("builder log on the SSH host:\n%s", logs)
		}
		_ = exec.Command("docker", "rm", "-f", id).Run()
		_ = exec.Command("docker", "volume", "rm", "-f", cacheVolume).Run()
	})
	port, err := exec.Command("docker", "inspect", "-f", `{{(index (index .NetworkSettings.Ports "22/tcp") 0).HostPort}}`, id).Output()
	if err != nil {
		t.Fatal(err)
	}
	var p int
	_, _ = fmt.Sscan(string(port), &p)
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, _, err := builders.ScanHostKey(context.Background(), "127.0.0.1", p); err == nil {
			return id, p
		}
		if time.Now().After(deadline) {
			t.Fatal("sshd did not start")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// enrolOverSSH returns the steps it saw and the stream's error.
func enrolOverSSH(c *client, req map[string]any) ([]string, *v1.EnrolOverSSHResponse, error) {
	cl := stackwrightv1connect.NewBuildServerServiceClient(c.http, c.w.srv.URL, connect.WithInterceptors(csrfHeader{c}))
	msg := &v1.EnrolOverSSHRequest{Org: "a", Name: req["name"].(string), Host: "127.0.0.1", Port: int32(req["port"].(int)),
		User: "root", HostKey: req["hostKey"].(string), Slots: 1, Labels: map[string]string{"host": "ssh"}}
	stream, err := cl.EnrolOverSSH(context.Background(), connect.NewRequest(msg))
	if err != nil {
		return nil, nil, err
	}
	var steps []string
	var last *v1.EnrolOverSSHResponse
	for stream.Receive() {
		last = stream.Msg()
		steps = append(steps, last.GetStep())
	}
	return steps, last, stream.Err()
}

func TestBuildServerEnrolOverSSH(t *testing.T) {
	t.Parallel()
	requireDocker(t)
	d, err := sandbox.NewDocker(sandbox.DockerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	platform, err := d.Platform(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(bin, "stackwright-builder-"+platform), "../../cmd/stackwright-builder")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+strings.TrimPrefix(platform, "linux_"))
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the builder: %v: %s", err, out)
	}
	binaries, err := builders.LoadBinaries(bin)
	if err != nil {
		t.Fatal(err)
	}
	w := startWith(t, nil, true, binaries)
	owner := w.setup()
	owner.must("ProjectService/CreateSampleProject", map[string]any{"org": "a"})
	rw := &runWorld{world: w, owner: owner}
	public := owner.must("BuildServerService/GetSSHKey", map[string]any{"org": "a"})["publicKey"].(string)
	volume := "stackwright-test-cache-" + strings.ToLower(rand.Text()[:8])
	host, port := sshHost(t, public, volume, false)

	scan := owner.must("BuildServerService/ScanHostKey", map[string]any{"org": "a", "host": "127.0.0.1", "port": port})
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	other, _ := ssh.NewPublicKey(pub)
	wrong := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(other)))
	if _, _, err := enrolOverSSH(owner, map[string]any{"name": "wrong", "port": port, "hostKey": wrong}); connect.CodeOf(err) != connect.CodeFailedPrecondition ||
		!strings.Contains(err.Error(), "does not match") {
		t.Errorf("wrong host key: %v", err)
	}
	if err := exec.Command("docker", "exec", host, "test", "-e", "/usr/local/bin/stackwright-builder").Run(); err == nil {
		t.Error("the builder was installed after a wrong host key")
	}

	steps, last, err := enrolOverSSH(owner, map[string]any{"name": "ssh-host", "port": port, "hostKey": scan["hostKey"]})
	if err != nil {
		t.Fatalf("enrol over SSH: %v (steps %v)", err, steps)
	}
	if !last.GetDone() || last.GetBuildServer().GetStatus() != v1.BuildServer_STATUS_ONLINE || !slices.Contains(steps, "Running a test sandbox") {
		t.Fatalf("enrol over SSH: steps %v, last %v", steps, last)
	}

	w.project(owner, "a", "net", "prod")
	prod := map[string]any{"org": "a", "project": "net", "workspace": "prod"}
	owner.must("ProjectService/UpdateWorkspace", merge(prod, map[string]any{"requiredLabels": map[string]any{"values": map[string]string{"host": "ssh"}}}))
	plan := func(number int) map[string]any {
		owner.must("RunService/CreateRun", merge(prod, map[string]any{"planOnly": true,
			"source": base64.StdEncoding.EncodeToString(source(t, "providers")), "sourceName": "providers.tar.gz"}))
		run := rw.waitFor(prod, number, "RUN_STATUS_PLANNED", "RUN_STATUS_FAILED")
		if run["status"] != "RUN_STATUS_PLANNED" || run["buildServer"] != "ssh-host" {
			t.Fatalf("plan %d on the SSH host: %v", number, run)
		}
		return run
	}
	plan(1)
	plan(2)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		out, _ := exec.Command("docker", "exec", host, "sh", "-c", "ls /var/cache/stackwright-builder/providers/*/hashicorp/random/").CombinedOutput()
		if strings.Contains(string(out), ".zip") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the random provider was never cached: %s", out)
		}
		time.Sleep(time.Second)
	}

	run := plan(3)
	var cached any
	var installs []string
	rows, err := w.owner.Query(context.Background(), `SELECT e.type, e.payload FROM job_events e JOIN runs r ON r.plan_job_id = e.job_id WHERE r.id = $1`, run["id"])
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var typ string
		var payload map[string]any
		if err := rows.Scan(&typ, &payload); err != nil {
			t.Fatal(err)
		}
		if typ == "executor.engine_ready" {
			cached = payload["cached"]
		}
		if msg, _ := payload["@message"].(string); strings.Contains(msg, "random") && strings.Contains(msg, "nstall") {
			installs = append(installs, msg)
		}
	}
	rows.Close()
	if cached != true {
		t.Errorf("plan 3 engine cached: %v", cached)
	}
	if len(installs) == 0 || !strings.Contains(strings.Join(installs, " "), "unauthenticated") {
		t.Errorf("plan 3 did not install random from the cache: %q", installs)
	}
}

func merge(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func TestBuildServerEnrolNeedsDocker(t *testing.T) {
	t.Parallel()
	requireDocker(t)
	w := startWith(t, nil, true, map[string]builders.Binary{"linux_amd64": {Path: "/dev/null"}, "linux_arm64": {Path: "/dev/null"}})
	owner := w.setup()
	public := owner.must("BuildServerService/GetSSHKey", map[string]any{"org": "a"})["publicKey"].(string)
	volume := "stackwright-test-cache-" + strings.ToLower(rand.Text()[:8])
	host, port := sshHost(t, public, volume, true)
	scan := owner.must("BuildServerService/ScanHostKey", map[string]any{"org": "a", "host": "127.0.0.1", "port": port})
	_, _, err := enrolOverSSH(owner, map[string]any{"name": "no-docker", "port": port, "hostKey": scan["hostKey"]})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "no Docker on") {
		t.Errorf("no Docker: %v", err)
	}
	if err := exec.Command("docker", "exec", host, "test", "-e", "/usr/local/bin/stackwright-builder").Run(); err == nil {
		t.Error("the builder was installed on a host with no Docker")
	}
}

// fakeSSHHost offers an SSH host key and nothing else, which is enough for
// ScanHostKey and for a host key check.
func fakeSSHHost(t *testing.T) int {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_, _, _, _ = ssh.NewServerConn(c, cfg)
			}()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}
