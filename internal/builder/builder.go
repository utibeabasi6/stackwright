// Package builder runs on a build server. It holds a stream to the control
// plane and starts a sandbox for each job it is sent (adr/0007, adr/0008).
package builder

import (
	"context"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/gen/stackwright/v1/stackwrightv1connect"
	"github.com/stackwright/stackwright/internal/sandbox"
)

const (
	heartbeatEvery = 15 * time.Second
	stopGrace      = 60 * time.Second
)

type Builder struct {
	Client  stackwrightv1connect.BuilderServiceClient
	Token   string
	Name    string
	Slots   int32
	Version string
	Backend sandbox.Backend
	Image   string
	// ServerURL is the server's address as sandboxes reach it.
	ServerURL string

	mu      sync.Mutex
	running map[string]sandbox.Sandbox
	// statuses wait here while the stream is down.
	statuses chan *v1.SandboxStatus
}

// Run keeps a stream open until ctx ends. Sandboxes outlive a dropped
// stream.
func (b *Builder) Run(ctx context.Context) {
	b.running = map[string]sandbox.Sandbox{}
	b.statuses = make(chan *v1.SandboxStatus, 256)
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
	stream := b.Client.Connect(streamCtx)
	stream.RequestHeader().Set("Authorization", "Bearer "+b.Token)
	go func() {
		<-streamCtx.Done()
		_ = stream.CloseRequest()
		_ = stream.CloseResponse()
	}()
	b.mu.Lock()
	running := slices.Sorted(maps.Keys(b.running))
	b.mu.Unlock()
	if err := stream.Send(&v1.ConnectRequest{Body: &v1.ConnectRequest_Hello{Hello: &v1.BuilderHello{
		Name: b.Name, Platform: platform, Slots: b.Slots, Version: b.Version, RunningJobs: running,
	}}}); err != nil {
		return err
	}
	slog.Info("builder connected", "name", b.Name, "platform", platform, "slots", b.Slots)

	go func() {
		tick := time.NewTicker(heartbeatEvery)
		defer tick.Stop()
		for {
			var msg *v1.ConnectRequest
			select {
			case <-streamCtx.Done():
				return
			case <-tick.C:
				msg = &v1.ConnectRequest{Body: &v1.ConnectRequest_Heartbeat{Heartbeat: &v1.BuilderHeartbeat{}}}
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
			go b.start(ctx, msg.GetStart().GetJobId(), msg.GetStart().GetTicket())
		case msg.GetStop() != nil:
			go b.stop(ctx, msg.GetStop().GetJobId())
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

func (b *Builder) start(ctx context.Context, jobID, ticket string) {
	s, err := b.Backend.Start(ctx, sandbox.Spec{
		Image:  b.Image,
		Env:    map[string]string{"STACKWRIGHT_SERVER_URL": b.ServerURL},
		Files:  map[string][]byte{"ticket": []byte(ticket)},
		Labels: map[string]string{"dev.stackwright.job": jobID},
	})
	if err != nil {
		slog.Error("start sandbox", "job", jobID, "err", err)
		b.report(&v1.SandboxStatus{JobId: jobID, State: v1.SandboxStatus_STATE_FAILED, Error: err.Error()})
		return
	}
	b.mu.Lock()
	b.running[jobID] = s
	b.mu.Unlock()
	b.report(&v1.SandboxStatus{JobId: jobID, State: v1.SandboxStatus_STATE_STARTED})
	slog.Info("sandbox started", "job", jobID, "sandbox", s.ID())

	code, err := s.Wait(ctx)
	if err != nil && ctx.Err() == nil {
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
	b.mu.Unlock()
	if s == nil {
		return
	}
	if err := s.Stop(ctx, stopGrace); err != nil {
		slog.Error("stop sandbox", "job", jobID, "err", err)
	}
}

// Dial returns a client for serverURL. The stream needs HTTP/2, which a
// plain http:// URL gets without TLS.
func Dial(serverURL string) stackwrightv1connect.BuilderServiceClient {
	var protocols http.Protocols
	protocols.SetHTTP2(true)
	protocols.SetUnencryptedHTTP2(true)
	return stackwrightv1connect.NewBuilderServiceClient(&http.Client{Transport: &http.Transport{Protocols: &protocols}}, serverURL)
}
