package sandbox_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/utibeabasi6/stackwright/internal/sandbox"
)

const (
	busybox  = "busybox:1.37.0"
	executor = "stackwright/executor:test"
)

func docker(t *testing.T) *sandbox.Docker {
	t.Helper()
	if os.Getenv("STACKWRIGHT_TEST_ENGINES") == "" {
		t.Skip("set STACKWRIGHT_TEST_ENGINES=1 to run sandboxes in Docker")
	}
	if out, err := exec.Command("docker", "pull", "-q", busybox).CombinedOutput(); err != nil {
		t.Fatalf("pull %s: %v: %s", busybox, err, out)
	}
	if out, err := exec.Command("docker", "build", "-q", "--target", "executor", "-t", executor, "../..").CombinedOutput(); err != nil {
		t.Fatalf("build the executor image: %v: %s", err, out)
	}
	d, err := sandbox.NewDocker(sandbox.DockerConfig{EgressImage: executor})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func run(t *testing.T, d *sandbox.Docker, spec sandbox.Spec) (sandbox.Sandbox, int32, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s, err := d.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Remove(context.Background()) })
	code, err := s.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	logs, err := s.Logs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return s, code, string(logs)
}

func inspect(t *testing.T, format, id string) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "-f", format, id).CombinedOutput()
	if err != nil {
		t.Fatalf("docker inspect %s: %v: %s", id, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestDockerSandboxIsLockedDown(t *testing.T) {
	d := docker(t)
	secret := "ticket-" + strings.Repeat("x", 20)
	s, code, logs := run(t, d, sandbox.Spec{
		Image: busybox,
		Command: []string{"sh", "-c", `id -u; cat /run/stackwright/ticket; echo; touch /root-write 2>/dev/null || echo read-only;
			touch /work/ok && echo work-writable; grep CapEff /proc/self/status; echo "env=$PUBLIC"`},
		Env:   map[string]string{"PUBLIC": "visible"},
		Files: map[string][]byte{"ticket": []byte(secret)},
	})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, logs)
	}
	for _, want := range []string{"65532\n", secret + "\n", "read-only\n", "work-writable\n", "CapEff:\t0000000000000000\n", "env=visible\n"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %q:\n%s", want, logs)
		}
	}

	all := inspect(t, "{{json .}}", s.ID())
	if strings.Contains(all, secret) {
		t.Error("docker inspect shows the ticket")
	}
	for format, want := range map[string]string{
		"{{.HostConfig.ReadonlyRootfs}}": "true",
		"{{.HostConfig.SecurityOpt}}":    "[no-new-privileges]",
		"{{.HostConfig.CapDrop}}":        "[ALL]",
		"{{.HostConfig.PidsLimit}}":      "1024",
		"{{.HostConfig.Memory}}":         "4294967296",
		"{{.HostConfig.NetworkMode}}":    s.ID(),
	} {
		if got := inspect(t, format, s.ID()); got != want {
			t.Errorf("%s = %s, want %s", format, got, want)
		}
	}

	if err := s.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"container", "network"} {
		if out, err := exec.Command("docker", kind, "inspect", s.ID()).CombinedOutput(); err == nil {
			t.Errorf("the %s is still there: %s", kind, out)
		}
	}
}

func TestDockerSandboxMissingImage(t *testing.T) {
	d := docker(t)
	_, err := d.Start(context.Background(), sandbox.Spec{Image: "stackwright/does-not-exist:none"})
	if !errors.Is(err, sandbox.ErrNoSuchImage) {
		t.Fatalf("got %v", err)
	}
}

func TestDockerSandboxCacheIsReadOnly(t *testing.T) {
	volume := "stackwright-test-cache-" + strings.ToLower(time.Now().Format("150405.000000"))
	volume = strings.ReplaceAll(volume, ".", "")
	t.Cleanup(func() { _ = exec.Command("docker", "volume", "rm", "-f", volume).Run() })
	d := docker(t)
	cached, err := sandbox.NewDocker(sandbox.DockerConfig{Cache: volume, EgressImage: executor})
	if err != nil {
		t.Fatal(err)
	}
	_, code, logs := run(t, cached, sandbox.Spec{Image: busybox, Cache: sandbox.CacheReadOnly,
		Command: []string{"sh", "-c", `ls -d /cache && (touch /cache/x 2>/dev/null && echo wrote || echo cache-read-only)`}})
	if code != 0 || !strings.Contains(logs, "cache-read-only") {
		t.Errorf("read-only cache: exit %d: %s", code, logs)
	}
	_, _, logs = run(t, d, sandbox.Spec{Image: busybox, Command: []string{"sh", "-c", `ls -d /cache 2>/dev/null || echo no-cache`}})
	if !strings.Contains(logs, "no-cache") {
		t.Errorf("a sandbox with no cache sees one: %s", logs)
	}
	if _, err := d.Start(context.Background(), sandbox.Spec{Image: busybox, Cache: sandbox.CacheReadOnly}); err == nil {
		t.Error("a builder with no cache started a sandbox that wants one")
	}
}

func TestDockerPullsAMissingImageOnlyFromANamedRegistry(t *testing.T) {
	const image = "docker.io/library/busybox:1.36.1"
	d := docker(t)
	_ = exec.Command("docker", "rmi", image).Run()
	if _, err := d.Start(context.Background(), sandbox.Spec{Image: "busybox:1.36.1"}); !errors.Is(err, sandbox.ErrNoSuchImage) {
		t.Errorf("a missing image with no registry in its name: %v", err)
	}
	_, code, logs := run(t, d, sandbox.Spec{Image: image, Command: []string{"true"}})
	if code != 0 {
		t.Errorf("exit %d: %s", code, logs)
	}
	if _, err := d.Start(context.Background(), sandbox.Spec{Image: "registry.invalid/stackwright/no-such-image:missing"}); err == nil {
		t.Error("an image no registry has started")
	}
}
