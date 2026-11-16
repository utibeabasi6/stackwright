// Package builder runs on a build server. It holds a stream to the control
// plane, starts a sandbox for each job it is sent, and keeps a cache of
// engines and providers (adr/0007, adr/0021).
package builder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	v1 "github.com/utibeabasi6/stackwright/gen/stackwright/v1"
	"github.com/utibeabasi6/stackwright/internal/builders"
	"github.com/utibeabasi6/stackwright/internal/sandbox"
)

const (
	heartbeatEvery = 15 * time.Second
	stopGrace      = 60 * time.Second
	maxEngineSize  = 512 << 20
)

var (
	validSHA      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	validEngine   = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	validProvider = regexp.MustCompile(`^([a-z0-9.-]+)/([a-z0-9_-]+)/([a-z0-9_-]+) ([0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.+-]*)$`)
)

type Builder struct {
	Identity *Identity
	Version  string
	Backend  sandbox.Backend
	// BackendName tells the server which executor image to send.
	BackendName string
	// Cache is the builder's own path to its cache, or empty for none.
	Cache string

	mu      sync.Mutex
	running map[string]sandbox.Sandbox
	// stopped holds stops that came before their sandbox started.
	stopped  map[string]bool
	statuses chan *v1.SandboxStatus
	mirror   sync.Mutex
}

// Run keeps a stream open until ctx ends. Sandboxes outlive a dropped
// stream.
func (b *Builder) Run(ctx context.Context) {
	b.running = map[string]sandbox.Sandbox{}
	b.stopped = map[string]bool{}
	b.statuses = make(chan *v1.SandboxStatus, 256)
	// A sandbox outlives the builder process too: the next one watches it.
	adopted, err := b.Backend.Adopt(ctx, b.Identity.ServerID)
	if err != nil {
		slog.Error("find the sandboxes of an earlier run", "err", err)
	}
	for jobID, s := range adopted {
		b.running[jobID] = s
		go b.watch(ctx, jobID, s)
	}
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := b.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		slog.Warn("builder stream ended; reconnecting", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (b *Builder) session(ctx context.Context) error {
	platform, err := b.Backend.Platform(ctx)
	if err != nil {
		return err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := b.Identity.Client().Connect(streamCtx)
	go func() {
		<-streamCtx.Done()
		_ = stream.CloseRequest()
		_ = stream.CloseResponse()
	}()
	b.mu.Lock()
	running := slices.Sorted(maps.Keys(b.running))
	b.mu.Unlock()
	if err := stream.Send(&v1.ConnectRequest{Body: &v1.ConnectRequest_Hello{Hello: &v1.BuilderHello{
		Platform: platform, Version: b.Version, RunningJobs: running, Backend: b.BackendName, Features: []string{builders.Toolbox},
	}}}); err != nil {
		return err
	}
	slog.Info("builder connected", "name", b.Identity.Name, "platform", platform)

	go func() {
		tick := time.NewTicker(heartbeatEvery)
		defer tick.Stop()
		// A renewal the server never answers is asked for again.
		var renewed time.Time
		for {
			var msg *v1.ConnectRequest
			select {
			case <-streamCtx.Done():
				return
			case <-tick.C:
				msg = &v1.ConnectRequest{Body: &v1.ConnectRequest_Heartbeat{Heartbeat: &v1.BuilderHeartbeat{}}}
				cert := b.Identity.certificate()
				if time.Since(renewed) < 5*time.Minute || time.Until(cert.NotAfter) > cert.NotAfter.Sub(cert.NotBefore)/2 {
					break
				}
				csr, err := b.Identity.csr()
				if err != nil {
					slog.Error("make a certificate request", "err", err)
					break
				}
				renewed = time.Now()
				msg = &v1.ConnectRequest{Body: &v1.ConnectRequest_Renew{Renew: &v1.RenewCertificate{Csr: csr}}}
			case st := <-b.statuses:
				msg = &v1.ConnectRequest{Body: &v1.ConnectRequest_Sandbox{Sandbox: st}}
			}
			if err := stream.Send(msg); err != nil {
				if st := msg.GetSandbox(); st != nil {
					b.report(st)
				}
				cancel()
				return
			}
		}
	}()

	for {
		msg, err := stream.Receive()
		if err != nil {
			return err
		}
		switch {
		case msg.GetStart() != nil:
			go b.start(ctx, msg.GetStart())
		case msg.GetStop() != nil:
			go b.stop(ctx, msg.GetStop().GetJobId())
		case msg.GetCertificate() != nil:
			if err := b.Identity.setCertificate(msg.GetCertificate()); err != nil {
				slog.Error("store the renewed certificate", "err", err)
				continue
			}
			slog.Info("certificate renewed", "until", b.Identity.certificate().NotAfter)
		}
	}
}

func (b *Builder) report(st *v1.SandboxStatus) {
	select {
	case b.statuses <- st:
	default:
		slog.Error("sandbox status dropped; the queue is full", "job", st.GetJobId())
	}
}

func (b *Builder) start(ctx context.Context, order *v1.StartSandbox) {
	jobID := order.GetJobId()
	spec := sandbox.Spec{Image: order.GetImage(), Labels: map[string]string{"dev.stackwright.job": jobID, sandbox.BuilderLabel: b.Identity.ServerID}}
	if u, err := url.Parse(order.GetServerUrl()); err == nil && u.Host != "" {
		port := u.Port()
		if port == "" {
			port = map[string]string{"https": "443"}[u.Scheme]
			if port == "" {
				port = "80"
			}
		}
		spec.Allow = []string{net.JoinHostPort(u.Hostname(), port)}
	}
	if order.GetTest() {
		spec.Command = []string{"version"}
	} else {
		spec.Env = map[string]string{"STACKWRIGHT_SERVER_URL": order.GetServerUrl()}
		spec.Files = map[string][]byte{"ticket": []byte(order.GetTicket())}
		if order.GetToolbox() {
			spec.Command = []string{"toolbox"}
		}
		if b.Cache != "" && !order.GetToolbox() {
			if err := b.cacheEngine(ctx, order.GetEngine()); err != nil {
				slog.Warn("cache the engine; the sandbox downloads it", "job", jobID, "err", err)
			}
			spec.Cache = sandbox.CacheReadOnly
		}
	}
	s, err := b.Backend.Start(ctx, spec)
	if err != nil {
		slog.Error("start sandbox", "job", jobID, "err", err)
		b.report(&v1.SandboxStatus{JobId: jobID, State: v1.SandboxStatus_STATE_FAILED, Error: err.Error()})
		return
	}
	b.mu.Lock()
	b.running[jobID] = s
	stopped := b.stopped[jobID]
	delete(b.stopped, jobID)
	b.mu.Unlock()
	if stopped {
		if err := s.Stop(ctx, stopGrace); err != nil {
			slog.Error("stop sandbox", "job", jobID, "err", err)
		}
	}
	b.report(&v1.SandboxStatus{JobId: jobID, State: v1.SandboxStatus_STATE_STARTED})
	slog.Info("sandbox started", "job", jobID, "sandbox", s.ID())
	if b.Cache != "" && len(order.GetProviders()) > 0 {
		go b.cacheProviders(ctx, order)
	}
	b.watch(ctx, jobID, s)
}

// watch removes a sandbox when it exits and reports the exit. When the
// builder stops first, the sandbox keeps running for the next builder.
func (b *Builder) watch(ctx context.Context, jobID string, s sandbox.Sandbox) {
	code, err := s.Wait(ctx)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		slog.Error("wait for sandbox", "job", jobID, "err", err)
	}
	if code != 0 {
		if logs, err := s.Logs(context.WithoutCancel(ctx)); err == nil {
			slog.Warn("sandbox failed", "job", jobID, "code", code, "logs", string(logs[max(0, len(logs)-4096):]))
		}
	}
	if err := s.Remove(context.WithoutCancel(ctx)); err != nil {
		slog.Error("remove sandbox", "job", jobID, "err", err)
	}
	b.mu.Lock()
	delete(b.running, jobID)
	b.mu.Unlock()
	b.report(&v1.SandboxStatus{JobId: jobID, State: v1.SandboxStatus_STATE_EXITED, ExitCode: code})
}

func (b *Builder) stop(ctx context.Context, jobID string) {
	b.mu.Lock()
	s := b.running[jobID]
	if s == nil {
		b.stopped[jobID] = true
	}
	b.mu.Unlock()
	if s == nil {
		return
	}
	if err := s.Stop(ctx, stopGrace); err != nil {
		slog.Error("stop sandbox", "job", jobID, "err", err)
	}
}

func (b *Builder) enginePath(d *v1.Download) (string, error) {
	if !validSHA.MatchString(d.GetSha256()) || !validEngine.MatchString(d.GetName()) {
		return "", fmt.Errorf("engine %q with SHA-256 %q", d.GetName(), d.GetSha256())
	}
	return filepath.Join(b.Cache, "engines", d.GetSha256(), d.GetName()), nil
}

// cacheEngine downloads the engine once and keeps it under its SHA-256.
func (b *Builder) cacheEngine(ctx context.Context, d *v1.Download) error {
	path, err := b.enginePath(d)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.GetUrl(), nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download the engine: %s", resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // sandboxes read the cache as another user
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".download-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxEngineSize+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		return err
	case n > maxEngineSize:
		return fmt.Errorf("the engine is larger than %d bytes", maxEngineSize)
	case hex.EncodeToString(h.Sum(nil)) != d.GetSha256():
		return errors.New("the engine does not match its SHA-256")
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil { //nolint:gosec // sandboxes run it as another user
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// cacheProviders mirrors the providers that are not cached yet, in a
// sandbox that runs only the engine's `providers mirror` and may write the
// cache. One runs at a time.
func (b *Builder) cacheProviders(ctx context.Context, order *v1.StartSandbox) {
	b.mirror.Lock()
	defer b.mirror.Unlock()
	platform, err := b.Backend.Platform(ctx)
	if err != nil {
		return
	}
	var missing []string
	for _, p := range order.GetProviders() {
		m := validProvider.FindStringSubmatch(p)
		if m == nil {
			slog.Warn("skip a provider that is not \"source version\"", "provider", p)
			continue
		}
		zip := fmt.Sprintf("terraform-provider-%s_%s_%s.zip", m[3], m[4], platform)
		if _, err := os.Stat(filepath.Join(b.Cache, "providers", m[1], m[2], m[3], zip)); err != nil {
			missing = append(missing, p)
		}
	}
	if len(missing) == 0 {
		return
	}
	path, err := b.enginePath(order.GetEngine())
	if err != nil {
		return
	}
	if _, err := os.Stat(path); err != nil {
		return
	}
	providers := filepath.Join(b.Cache, "providers")
	if err := os.MkdirAll(providers, 0o755); err != nil { //nolint:gosec // job sandboxes read it as another user
		slog.Warn("make the provider cache", "err", err)
		return
	}
	if err := os.Chown(providers, sandbox.UID, sandbox.UID); err != nil {
		slog.Warn("give the provider cache to the sandbox user", "err", err)
		return
	}
	s, err := b.Backend.Start(ctx, sandbox.Spec{
		Image:   order.GetImage(),
		Command: []string{"mirror"},
		Files: map[string][]byte{
			"engine":    []byte(order.GetEngine().GetSha256() + " " + order.GetEngine().GetName()),
			"providers": []byte(strings.Join(missing, "\n")),
		},
		Labels: map[string]string{"dev.stackwright.mirror": "true", sandbox.BuilderLabel: b.Identity.ServerID},
		Cache:  sandbox.CacheWritable,
	})
	if err != nil {
		slog.Warn("start the provider mirror", "err", err)
		return
	}
	defer func() { _ = s.Remove(context.WithoutCancel(ctx)) }()
	code, err := s.Wait(ctx)
	if err != nil || code != 0 {
		logs, _ := s.Logs(context.WithoutCancel(ctx))
		slog.Warn("provider mirror failed", "code", code, "err", err, "logs", string(logs[max(0, len(logs)-4096):]))
		return
	}
	slog.Info("providers cached", "providers", missing)
}
