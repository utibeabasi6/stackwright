package server_test

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stackwright/stackwright/internal/sandbox"
)

const probeImage = "stackwright/escape-probe:test"

// A sandbox reaches only the server's job API and public hosts, directly
// or through the egress proxy (adr/0046).
func TestSandboxesCannotEscape(t *testing.T) {
	t.Parallel()
	requireDocker(t)
	if out, err := exec.Command("docker", "build", "-q", "-t", probeImage, "testdata/escape-probe").CombinedOutput(); err != nil {
		t.Fatalf("build the probe image: %v: %s", err, out)
	}
	w := start(t, nil, true)
	server := strings.TrimPrefix(w.public, "http://")
	d, err := sandbox.NewDocker(sandbox.DockerConfig{ExtraHosts: []string{"host.docker.internal:host-gateway"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	start := func(script string) sandbox.Sandbox {
		s, err := d.Start(ctx, sandbox.Spec{Image: probeImage, Command: []string{script}, Allow: []string{server}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Remove(context.Background()) })
		return s
	}
	other := start("while true; do echo hi | nc -l -p 8000; done")
	ip, err := exec.Command("docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}", other.ID()).Output()
	if err != nil {
		t.Fatal(err)
	}
	otherIP := strings.Fields(string(ip))[0]

	// Each line is "name result". direct: a raw TCP connection with no
	// proxy. proxied: an HTTP request through the egress proxy.
	targets := map[string]string{
		"other-sandbox":  otherIP + ":8000",
		"metadata":       "169.254.169.254:80",
		"bridge-gateway": "172.17.0.1:80",
		"server-builder": "host.docker.internal:" + w.builderLocal[strings.LastIndex(w.builderLocal, ":")+1:],
		"postgres":       "host.docker.internal:15432",
		"nats":           "host.docker.internal:14222",
		"rustfs":         "host.docker.internal:19000",
	}
	var script strings.Builder
	for name, addr := range targets {
		host, p, _ := strings.Cut(addr, ":")
		fmt.Fprintf(&script, "if nc -z -w 3 %s %s 2>/dev/null; then echo 'direct-%s open'; else echo 'direct-%s closed'; fi; ", host, p, name, name)
		fmt.Fprintf(&script, "if wget -q -T 5 -O /dev/null http://%s/ 2>/tmp/e; then echo 'proxied-%s open'; else echo \"proxied-%s $(head -c 120 /tmp/e)\"; fi; ",
			addr, name, name)
	}
	fmt.Fprintf(&script, "wget -q -T 5 -O- http://%s/healthz && echo ' server-api'; ", server)
	fmt.Fprintf(&script, "wget -q -T 10 -O /dev/null http://example.com/ && echo 'public open'; ")
	probe := start(script.String())
	code, err := probe.Wait(ctx)
	logs, _ := probe.Logs(ctx)
	if err != nil {
		t.Fatalf("the probe: %v %d\n%s", err, code, logs)
	}
	out := string(logs)
	t.Log(out)
	for name := range targets {
		if !strings.Contains(out, "direct-"+name+" closed") {
			t.Errorf("a sandbox reached %s directly:\n%s", name, out)
		}
		if strings.Contains(out, "proxied-"+name+" open") || !strings.Contains(out, "proxied-"+name+" wget: server returned error: HTTP/1.1 403") {
			t.Errorf("the proxy did not refuse %s:\n%s", name, out)
		}
	}
	if !strings.Contains(out, "ok\n server-api") || !strings.Contains(out, "public open") {
		t.Errorf("the server's job API or a public host is out of reach:\n%s", out)
	}
}
