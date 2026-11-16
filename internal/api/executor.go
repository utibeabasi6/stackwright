package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/internal/authz"
	"github.com/stackwright/stackwright/internal/jobs"
)

const (
	maxEventsPerCall = 500
	maxEventPayload  = 1 << 20
	maxJobError      = 4000
)

type Executor struct {
	Jobs *jobs.Service
	// Changed tells watchers that a run has new events.
	Changed func(runID string)
}

func (e Executor) ClaimJob(ctx context.Context, _ *connect.Request[v1.ClaimJobRequest]) (*connect.Response[v1.ClaimJobResponse], error) {
	s := authz.ScopeFrom(ctx)
	job, err := e.Jobs.Claim(ctx, s.Tx, s.JobID)
	if errors.Is(err, jobs.ErrNotClaimable) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if err != nil {
		return nil, authz.Internal(err)
	}
	return connect.NewResponse(&v1.ClaimJobResponse{Job: job}), nil
}

func (e Executor) ReportEvents(ctx context.Context, req *connect.Request[v1.ReportEventsRequest]) (*connect.Response[v1.ReportEventsResponse], error) {
	s := authz.ScopeFrom(ctx)
	events := req.Msg.GetEvents()
	if len(events) > maxEventsPerCall {
		return nil, invalid(fmt.Sprintf("send at most %d events per call", maxEventsPerCall))
	}
	batch := make([]jobs.Event, len(events))
	for i, ev := range events {
		switch {
		case ev.GetSeq() <= 0:
			return nil, invalid("event seq starts at 1")
		case ev.GetType() == "" || len(ev.GetType()) > 100:
			return nil, invalid(fmt.Sprintf("event %d: bad type", ev.GetSeq()))
		case len(ev.GetResourceAddr()) > 1000:
			return nil, invalid(fmt.Sprintf("event %d: the resource address is too long", ev.GetSeq()))
		case !json.Valid(ev.GetPayload()):
			return nil, invalid(fmt.Sprintf("event %d: the payload must be JSON", ev.GetSeq()))
		}
		// One event the database cannot hold must not stop the job's other
		// events and heartbeats: jsonb refuses \u0000, and a line can be
		// larger than any timeline needs.
		payload := ev.GetPayload()
		switch {
		case len(payload) > maxEventPayload:
			payload = []byte(`{"dropped":"the event was larger than 1 MiB"}`)
		case bytes.Contains(payload, []byte(`\u0000`)):
			var v any
			_ = json.Unmarshal(payload, &v)
			payload, _ = json.Marshal(withoutNUL(v))
		}
		batch[i] = jobs.Event{Seq: ev.GetSeq(), Type: ev.GetType(), ResourceAddr: ev.GetResourceAddr(), Payload: string(payload), At: ev.GetAt().AsTime()}
	}
	if len(batch) > 0 {
		runID, err := jobs.RecordEvents(ctx, s.Tx, s.JobID, s.OrgID, batch)
		if err != nil {
			return nil, authz.Internal(err)
		}
		if runID != "" && e.Changed != nil {
			s.AfterCommit(func() { e.Changed(runID) })
		}
	}
	return connect.NewResponse(&v1.ReportEventsResponse{}), nil
}

func (e Executor) FinishJob(ctx context.Context, req *connect.Request[v1.FinishJobRequest]) (*connect.Response[v1.FinishJobResponse], error) {
	s := authz.ScopeFrom(ctx)
	msg := strings.ReplaceAll(req.Msg.GetError(), "\x00", "")
	if len(msg) > maxJobError {
		msg = strings.ToValidUTF8(msg[:maxJobError], "")
	}
	if err := e.Jobs.Finish(ctx, s.Tx, s.JobID, req.Msg.GetSucceeded(), msg, req.Msg.GetHasChanges()); err != nil {
		return nil, authz.Internal(err)
	}
	return connect.NewResponse(&v1.FinishJobResponse{}), nil
}

func withoutNUL(v any) any {
	switch v := v.(type) {
	case string:
		return strings.ReplaceAll(v, "\x00", "\uFFFD")
	case []any:
		for i := range v {
			v[i] = withoutNUL(v[i])
		}
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			out[strings.ReplaceAll(k, "\x00", "\uFFFD")] = withoutNUL(e)
		}
		return out
	}
	return v
}
