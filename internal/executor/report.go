package executor

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/gen/stackwright/v1/stackwrightv1connect"
)

const (
	// heartbeatEvery must stay well under the 45 s after which the server
	// counts a job as lost (internal/jobs).
	heartbeatEvery = 15 * time.Second
	sendEvery      = time.Second
	maxBatch       = 500
)

// reporter keeps events until the server stores them. The server ignores a
// seq it has, so a batch can be sent again after an error.
type reporter struct {
	client stackwrightv1connect.ExecutorServiceClient
	token  string
	// lost is called when the server no longer accepts the job token.
	lost func()

	sending sync.Mutex
	mu      sync.Mutex
	pending []*v1.JobEvent
	seq     int64
}

func (r *reporter) emit(typ, resourceAddr string, payload []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	r.pending = append(r.pending, &v1.JobEvent{Seq: r.seq, Type: typ, ResourceAddr: resourceAddr, Payload: payload, At: timestamppb.Now()})
}

// send sends one batch, or an empty one as a heartbeat. It reports whether
// events are still waiting.
func (r *reporter) send(ctx context.Context) (bool, error) {
	r.sending.Lock()
	defer r.sending.Unlock()
	r.mu.Lock()
	batch := r.pending[:min(len(r.pending), maxBatch)]
	r.mu.Unlock()
	req := connect.NewRequest(&v1.ReportEventsRequest{Events: batch})
	req.Header().Set("Authorization", "Bearer "+r.token)
	if _, err := r.client.ReportEvents(ctx, req); err != nil {
		switch connect.CodeOf(err) {
		case connect.CodeUnauthenticated:
			r.lost()
		case connect.CodeInvalidArgument:
			// Sending the same batch again cannot succeed, and holding it
			// would stop the heartbeats that keep the job alive.
			slog.Error("dropping events the server refused", "count", len(batch), "err", err)
			r.mu.Lock()
			r.pending = r.pending[len(batch):]
			more := len(r.pending) > 0
			r.mu.Unlock()
			return more, nil
		}
		return true, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending = r.pending[len(batch):]
	return len(r.pending) > 0, nil
}

func (r *reporter) run(ctx context.Context) {
	last := time.Now()
	tick := time.NewTicker(sendEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		r.mu.Lock()
		waiting := len(r.pending) > 0
		r.mu.Unlock()
		if !waiting && time.Since(last) < heartbeatEvery {
			continue
		}
		if _, err := r.send(ctx); err != nil {
			slog.Warn("report events", "err", err)
			continue
		}
		last = time.Now()
	}
}

func (r *reporter) flush(ctx context.Context) error {
	for {
		more, err := r.send(ctx)
		switch {
		case err != nil && (ctx.Err() != nil || connect.CodeOf(err) == connect.CodeUnauthenticated):
			return errors.Join(err, ctx.Err())
		case err != nil:
			slog.Warn("report events", "err", err)
			time.Sleep(sendEvery)
		case !more:
			return nil
		}
	}
}
