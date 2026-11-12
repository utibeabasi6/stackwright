// Package builders keeps the builders connected to this server and sends
// them sandbox orders (adr/0008).
package builders

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"

	"connectrpc.com/connect"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
)

var (
	ErrGone       = errors.New("the build server disconnected")
	validName     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	validPlatform = regexp.MustCompile(`^linux_(amd64|arm64)$`)
)

type Hub struct {
	// OnExit is called when a job's sandbox exits or cannot start.
	OnExit func(jobID string, s *v1.SandboxStatus)

	mu       sync.Mutex
	builders map[*builder]bool
}

type builder struct {
	name     string
	platform string
	slots    int
	reserved int
	jobs     map[string]bool
	send     chan *v1.ConnectResponse
	done     chan struct{}
}

func (b *builder) used() int { return b.reserved + len(b.jobs) }

// Slot is a place on a builder, held until Start or Release.
type Slot struct {
	h        *Hub
	b        *builder
	Builder  string
	Platform string
}

// Reserve holds a free slot on the least busy builder.
func (h *Hub) Reserve() (*Slot, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var best *builder
	for b := range h.builders {
		if b.used() < b.slots && (best == nil || b.used() < best.used()) {
			best = b
		}
	}
	if best == nil {
		return nil, false
	}
	best.reserved++
	return &Slot{h: h, b: best, Builder: best.name, Platform: best.platform}, true
}

func (s *Slot) Release() {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	s.b.reserved--
}

// Start turns the slot into a running job and sends the order.
func (s *Slot) Start(jobID, ticket string) error {
	s.h.mu.Lock()
	s.b.reserved--
	s.b.jobs[jobID] = true
	s.h.mu.Unlock()
	return s.h.send(s.b, &v1.ConnectResponse{Body: &v1.ConnectResponse_Start{Start: &v1.StartSandbox{JobId: jobID, Ticket: ticket}}})
}

// Stop asks the builder that runs jobID to stop its sandbox. A job on no
// connected builder is left to the reaper.
func (h *Hub) Stop(jobID string) {
	h.mu.Lock()
	var on *builder
	for b := range h.builders {
		if b.jobs[jobID] {
			on = b
		}
	}
	h.mu.Unlock()
	if on != nil {
		if err := h.send(on, &v1.ConnectResponse{Body: &v1.ConnectResponse_Stop{Stop: &v1.StopSandbox{JobId: jobID}}}); err != nil {
			slog.Warn("stop sandbox", "job", jobID, "err", err)
		}
	}
}

func (h *Hub) send(b *builder, msg *v1.ConnectResponse) error {
	select {
	case b.send <- msg:
		return nil
	case <-b.done:
		return ErrGone
	}
}

func (h *Hub) Connect(ctx context.Context, stream *connect.BidiStream[v1.ConnectRequest, v1.ConnectResponse]) error {
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	switch {
	case hello == nil:
		return connect.NewError(connect.CodeInvalidArgument, errors.New("the first message must be a hello"))
	case !validName.MatchString(hello.GetName()):
		return connect.NewError(connect.CodeInvalidArgument, errors.New("a builder name is lower-case letters, digits and hyphens"))
	case !validPlatform.MatchString(hello.GetPlatform()):
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("platform %q is not supported", hello.GetPlatform()))
	case hello.GetSlots() < 1 || hello.GetSlots() > 64:
		return connect.NewError(connect.CodeInvalidArgument, errors.New("a builder has 1 to 64 slots"))
	}
	b := &builder{
		name: hello.GetName(), platform: hello.GetPlatform(), slots: int(hello.GetSlots()),
		jobs: map[string]bool{}, send: make(chan *v1.ConnectResponse, 16), done: make(chan struct{}),
	}
	for _, j := range hello.GetRunningJobs() {
		b.jobs[j] = true
	}
	h.mu.Lock()
	if h.builders == nil {
		h.builders = map[*builder]bool{}
	}
	h.builders[b] = true
	h.mu.Unlock()
	slog.Info("builder connected", "name", b.name, "platform", b.platform, "slots", b.slots)
	defer func() {
		h.mu.Lock()
		delete(h.builders, b)
		h.mu.Unlock()
		close(b.done)
		slog.Info("builder disconnected", "name", b.name)
	}()

	ctx, cancel := context.WithCancel(ctx)
	sendErr := make(chan error, 1)
	sent := make(chan struct{})
	defer func() {
		cancel()
		<-sent
	}()
	go func() {
		defer close(sent)
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-b.send:
				if err := stream.Send(msg); err != nil {
					sendErr <- err
					cancel()
					return
				}
			}
		}
	}()
	for {
		msg, err := stream.Receive()
		if err != nil {
			select {
			case err = <-sendErr:
			default:
			}
			return err
		}
		st := msg.GetSandbox()
		if st == nil || (st.GetState() != v1.SandboxStatus_STATE_EXITED && st.GetState() != v1.SandboxStatus_STATE_FAILED) {
			continue
		}
		h.mu.Lock()
		known := b.jobs[st.GetJobId()]
		delete(b.jobs, st.GetJobId())
		h.mu.Unlock()
		if known && h.OnExit != nil {
			h.OnExit(st.GetJobId(), st)
		}
	}
}
