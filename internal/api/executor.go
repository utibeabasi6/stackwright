package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

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
}

func (e Executor) ClaimJob(ctx context.Context, _ *connect.Request[v1.ClaimJobRequest]) (*connect.Response[v1.ClaimJobResponse], error) {
	s := authz.ScopeFrom(ctx)
	job, err := e.Jobs.Claim(ctx, s.Tx, s.JobID)
	if err != nil {
		return nil, authz.Internal(err)
	}
	return connect.NewResponse(&v1.ClaimJobResponse{Job: job}), nil
}

func (Executor) ReportEvents(ctx context.Context, req *connect.Request[v1.ReportEventsRequest]) (*connect.Response[v1.ReportEventsResponse], error) {
	s := authz.ScopeFrom(ctx)
	events := req.Msg.GetEvents()
	if len(events) > maxEventsPerCall {
		return nil, invalid(fmt.Sprintf("send at most %d events per call", maxEventsPerCall))
	}
	n := len(events)
	seqs, types, addrs, payloads, ats := make([]int64, n), make([]string, n), make([]string, n), make([]string, n), make([]time.Time, n)
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
		seqs[i], types[i], addrs[i], payloads[i], ats[i] = ev.GetSeq(), ev.GetType(), ev.GetResourceAddr(), string(ev.GetPayload()), ev.GetAt().AsTime()
	}
	if n > 0 {
		if _, err := s.Tx.Exec(ctx, `INSERT INTO job_events (job_id, org_id, seq, type, resource_addr, payload, at)
			SELECT $1, $2, e.seq, e.type, e.addr, e.payload::jsonb, e.at
			FROM unnest($3::bigint[], $4::text[], $5::text[], $6::text[], $7::timestamptz[]) AS e (seq, type, addr, payload, at)
			ON CONFLICT (job_id, seq) DO NOTHING`, s.JobID, s.OrgID, seqs, types, addrs, payloads, ats); err != nil {
			return nil, authz.Internal(err)
		}
	}
	return connect.NewResponse(&v1.ReportEventsResponse{}), nil
}

func (Executor) FinishJob(ctx context.Context, req *connect.Request[v1.FinishJobRequest]) (*connect.Response[v1.FinishJobResponse], error) {
	s := authz.ScopeFrom(ctx)
	msg := req.Msg.GetError()
	if len(msg) > maxJobError {
		msg = msg[:maxJobError]
	}
	if err := jobs.Finish(ctx, s.Tx, s.JobID, req.Msg.GetSucceeded(), msg, req.Msg.GetHasChanges()); err != nil {
		return nil, authz.Internal(err)
	}
	return connect.NewResponse(&v1.FinishJobResponse{}), nil
}
