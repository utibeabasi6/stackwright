package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/internal/authz"
)

// Connect serves a session's toolbox. Its first order is the checkout; the
// toolbox answers ready, then takes calls.
func (s *Service) Connect(ctx context.Context, stream *connect.BidiStream[v1.ToolboxMessage, v1.ToolboxOrder]) error {
	sc := authz.ScopeFrom(ctx)
	id, orgID := sc.AgentSessionID, sc.OrgID
	l := s.session(id)
	checkout, err := s.checkoutOrder(ctx, id, orgID)
	if err != nil {
		return connectErr(err)
	}
	s.mu.Lock()
	l.conn++
	conn := l.conn
	orders := make(chan *v1.ToolboxOrder, 4)
	l.orders = orders
	s.mu.Unlock()
	if err := stream.Send(&v1.ToolboxOrder{Body: &v1.ToolboxOrder_Checkout{Checkout: checkout}}); err != nil {
		return err
	}
	received := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Receive()
			if err != nil {
				received <- err
				return
			}
			switch b := msg.GetBody().(type) {
			case *v1.ToolboxMessage_Ready:
				s.setDirty(ctx, orgID, id, b.Ready.GetDirty())
				s.mu.Lock()
				select {
				case <-l.ready:
				default:
					close(l.ready)
				}
				s.mu.Unlock()
				s.changed(id)
			case *v1.ToolboxMessage_Result:
				s.setDirty(ctx, orgID, id, b.Result.GetDirty())
				s.mu.Lock()
				ch := l.results[b.Result.GetId()]
				delete(l.results, b.Result.GetId())
				s.mu.Unlock()
				if ch != nil {
					ch <- b.Result
				}
			}
		}
	}()
	defer func() {
		s.mu.Lock()
		latest := l.conn == conn
		s.mu.Unlock()
		if latest {
			s.disconnected(id)
		}
	}()
	for {
		select {
		case o := <-orders:
			if err := stream.Send(o); err != nil {
				return err
			}
		case <-received:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

// disconnected makes later calls wait for the toolbox to come back, and
// ends the calls that were in flight.
func (s *Service) disconnected(id string) {
	l := s.session(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-l.ready:
		l.ready = make(chan struct{})
	default:
	}
	for callID, ch := range l.results {
		ch <- &v1.ToolResult{Id: callID, Error: true, Output: "the sandbox's connection dropped while this ran"}
		delete(l.results, callID)
	}
}

func (s *Service) checkoutOrder(ctx context.Context, id, orgID string) (*v1.Checkout, error) {
	l := s.session(id)
	s.mu.Lock()
	gitBase, platform, remote := l.gitBase, l.platform, l.remote
	s.mu.Unlock()
	if gitBase == "" {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("this server did not start the session's sandbox"))
	}
	c := &v1.Checkout{GitUrl: fmt.Sprintf("%s/git/%s/", gitBase, id)}
	var eng, version string
	err := s.inOrg(ctx, orgID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT s.branch, s.base, p.path, p.engine, p.engine_version FROM agent_sessions s
			JOIN workspaces w ON w.id = s.workspace_id JOIN projects p ON p.id = w.project_id WHERE s.id = $1`, id).
			Scan(&c.Branch, &c.Base, &c.Path, &eng, &version)
	})
	if err != nil {
		return nil, err
	}
	if c.Engine, err = s.Jobs.EngineDownload(ctx, eng, version, platform, remote); err != nil {
		return nil, err
	}
	c.EngineName = "tofu"
	if eng == "terraform" {
		c.EngineName = "terraform"
	}
	return c, nil
}

// order sends an order to the session's toolbox and waits for its result.
func (s *Service) order(ctx context.Context, id string, build func(callID string) *v1.ToolboxOrder) (*v1.ToolResult, error) {
	l := s.session(id)
	s.mu.Lock()
	ready := l.ready
	s.mu.Unlock()
	select {
	case <-ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(readyTimeout):
		return nil, ErrNoToolbox
	}
	callID := uuid.NewString()
	ch := make(chan *v1.ToolResult, 1)
	s.mu.Lock()
	l.results[callID] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(l.results, callID)
		s.mu.Unlock()
	}()
	s.mu.Lock()
	orders := l.orders
	s.mu.Unlock()
	select {
	case orders <- build(callID):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case r := <-ch:
		return r, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(toolTimeout):
		return nil, fmt.Errorf("the tool gave no result within %s", toolTimeout)
	}
}

func (s *Service) callTool(ctx context.Context, id, name, args string) (*v1.ToolResult, error) {
	return s.order(ctx, id, func(callID string) *v1.ToolboxOrder {
		return &v1.ToolboxOrder{Body: &v1.ToolboxOrder_Call{Call: &v1.ToolCall{Id: callID, Name: name, Arguments: args}}}
	})
}

// removeSandbox stops the session's sandbox and ends its token. The session
// sleeps, unless it is closed; its next message starts a fresh sandbox.
func (s *Service) removeSandbox(ctx context.Context, id, orgID, why string) {
	s.Hub.Stop(sandboxPrefix + id)
	err := s.inOrg(ctx, orgID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM agent_sandbox_tokens WHERE session_id = $1`, id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE agent_sessions SET sandbox_id = NULL, dirty = false, updated_at = now(),
			status = CASE WHEN status = 'closed' THEN 'closed' ELSE 'asleep' END WHERE id = $1`, id)
		return err
	})
	if err != nil {
		slog.Error("remove agent sandbox", "session", id, "err", err)
	}
	s.disconnected(id)
	slog.Info("agent sandbox removed", "session", id, "why", why)
	s.changed(id)
}

// SandboxExited is the hub's callback for a sandbox whose job id starts
// with "agent-".
func (s *Service) SandboxExited(jobID string, st *v1.SandboxStatus) {
	id, ok := strings.CutPrefix(jobID, sandboxPrefix)
	if !ok {
		return
	}
	l := s.session(id)
	s.mu.Lock()
	orgID := l.orgID
	s.mu.Unlock()
	if orgID == "" {
		return
	}
	s.removeSandbox(context.Background(), id, orgID, "exited: "+st.GetError())
}

// IsSandbox reports whether a hub job id is an agent sandbox's.
func IsSandbox(jobID string) bool { return strings.HasPrefix(jobID, sandboxPrefix) }

// RunReaper removes sandboxes with no uncommitted changes that saw no turn
// for IdleAfter. A sandbox with changes stays until a person commits or
// discards them (adr/0011).
func (s *Service) RunReaper(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := s.reap(ctx); err != nil {
			slog.Error("agent reaper", "err", err)
		}
	}
}

func (s *Service) reap(ctx context.Context) error {
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT session_id, org_id FROM agent_sandbox_tokens`)
	if err != nil {
		return err
	}
	type running struct{ id, org string }
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (running, error) {
		var x running
		return x, r.Scan(&x.id, &x.org)
	})
	if err != nil {
		return err
	}
	for _, r := range list {
		var idle bool
		err := s.inOrg(ctx, r.org, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT NOT dirty AND status NOT IN ('working', 'starting') AND idle_since < now() - make_interval(secs => $2)
				FROM agent_sessions WHERE id = $1`, r.id, s.IdleAfter.Seconds()).Scan(&idle)
		})
		if err != nil {
			return err
		}
		l := s.session(r.id)
		s.mu.Lock()
		busy := l.turn != nil
		s.mu.Unlock()
		if idle && !busy {
			s.removeSandbox(ctx, r.id, r.org, "idle")
		}
	}
	return nil
}
