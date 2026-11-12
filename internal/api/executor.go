package api

import (
	"context"
	"encoding/json"
	"fmt"

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
		case len(ev.GetPayload()) > maxEventPayload || !json.Valid(ev.GetPayload()):
			return nil, invalid(fmt.Sprintf("event %d: the payload must be JSON of at most 1 MiB", ev.GetSeq()))
		}
		batch[i] = jobs.Event{Seq: ev.GetSeq(), Type: ev.GetType(), ResourceAddr: ev.GetResourceAddr(), Payload: string(ev.GetPayload()), At: ev.GetAt().AsTime()}
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
	msg := req.Msg.GetError()
	if len(msg) > maxJobError {
		msg = msg[:maxJobError]
	}
	if err := e.Jobs.Finish(ctx, s.Tx, s.JobID, req.Msg.GetSucceeded(), msg, req.Msg.GetHasChanges()); err != nil {
		return nil, authz.Internal(err)
	}
	return connect.NewResponse(&v1.FinishJobResponse{}), nil
}
