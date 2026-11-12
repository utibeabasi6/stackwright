package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/internal/auth"
	"github.com/stackwright/stackwright/internal/authz"
	"github.com/stackwright/stackwright/internal/toolbox/guard"
)

const (
	ticketLife = 30 * time.Second
	maxFrame   = 8 << 20
)

// ErrStarting is returned while the session's sandbox starts; the editor
// tries again.
var ErrStarting = errors.New("the session's sandbox is starting; try again in a moment")

type ticket struct {
	session, org string
	expires      time.Time
}

// editorReady reports whether the session's toolbox can take an order. If
// it cannot, it starts the sandbox in the background and returns
// ErrStarting, so no request waits for a sandbox to start (adr/0034).
func (s *Service) editorReady(ctx context.Context, tx pgx.Tx, id, orgID string) error {
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM agent_sessions WHERE id = $1`, id).Scan(&status); err != nil {
		return err
	}
	if status == "closed" {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("the session is closed"))
	}
	l := s.session(id)
	s.mu.Lock()
	ready, busy := l.ready, l.turn != nil
	s.mu.Unlock()
	select {
	case <-ready:
		return nil
	default:
	}
	// The error rolls the request's transaction back, so the start runs on
	// its own.
	if !busy {
		go func() {
			bg := context.Background()
			err := s.ensureSandbox(bg, id, orgID)
			// ensureSandbox marks the session starting; it goes back to what
			// it was, unless a turn took it meanwhile.
			_ = s.inOrg(bg, orgID, func(tx pgx.Tx) error {
				_, err := tx.Exec(bg, `UPDATE agent_sessions SET status = $2, updated_at = now() WHERE id = $1 AND status = 'starting'`, id, status)
				return err
			})
			s.changed(id)
			if err != nil {
				slog.Warn("start editor sandbox", "session", id, "err", err)
			}
		}()
	}
	return connect.NewError(connect.CodeUnavailable, ErrStarting)
}

func (s *Service) ListFiles(ctx context.Context, req *connect.Request[v1.ListFilesRequest]) (*connect.Response[v1.ListFilesResponse], error) {
	sc := authz.ScopeFrom(ctx)
	if err := s.editorReady(ctx, sc.Tx, req.Msg.Id, sc.OrgID); err != nil {
		return nil, connectErr(err)
	}
	r, err := s.callTool(ctx, req.Msg.Id, "list_files", "")
	if err == nil && r.GetError() {
		err = errors.New(r.GetOutput())
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	var paths []string
	for _, p := range strings.Split(r.GetOutput(), "\n") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return connect.NewResponse(&v1.ListFilesResponse{Paths: paths}), nil
}

func (s *Service) OpenFile(ctx context.Context, req *connect.Request[v1.OpenFileRequest]) (*connect.Response[v1.OpenFileResponse], error) {
	sc := authz.ScopeFrom(ctx)
	m := req.Msg
	if why := guard.Refused(m.Path); why != "" {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s: %s", m.Path, why))
	}
	if err := s.editorReady(ctx, sc.Tx, m.Id, sc.OrgID); err != nil {
		return nil, connectErr(err)
	}
	args, _ := json.Marshal(map[string]string{"path": m.Path})
	r, err := s.callTool(ctx, m.Id, "open_file", string(args))
	if err == nil && r.GetError() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(r.GetOutput()))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	return connect.NewResponse(&v1.OpenFileResponse{Content: string(r.GetData())}), nil
}

// SaveFile writes the file in the checkout. The toolbox takes one order at
// a time, so during a turn the save lands between two tool calls. The
// model gets an edit step, which tells it to read the file again.
func (s *Service) SaveFile(ctx context.Context, req *connect.Request[v1.SaveFileRequest]) (*connect.Response[v1.SaveFileResponse], error) {
	sc := authz.ScopeFrom(ctx)
	m := req.Msg
	if why := guard.Refused(m.Path); why != "" {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s: %s", m.Path, why))
	}
	if err := s.editorReady(ctx, sc.Tx, m.Id, sc.OrgID); err != nil {
		return nil, connectErr(err)
	}
	args, _ := json.Marshal(map[string]string{"path": m.Path, "base": m.Base, "content": m.Content})
	r, err := s.callTool(ctx, m.Id, "save_file", string(args))
	if err == nil && r.GetError() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(r.GetOutput()))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	sc.Target(fmt.Sprintf("file %s in agent session %s", m.Path, m.Id))
	outcome := map[string]v1.SaveOutcome{"saved": v1.SaveOutcome_SAVE_OUTCOME_SAVED, "merged": v1.SaveOutcome_SAVE_OUTCOME_MERGED,
		"conflict": v1.SaveOutcome_SAVE_OUTCOME_CONFLICT}[r.GetOutput()]
	if outcome != v1.SaveOutcome_SAVE_OUTCOME_CONFLICT {
		var name string
		if err := sc.Tx.QueryRow(ctx, `SELECT coalesce(nullif(name, ''), email) FROM users WHERE id = $1`, sc.UserID).Scan(&name); err != nil {
			return nil, authz.Internal(err)
		}
		if err := appendStep(ctx, sc.Tx, sc.OrgID, m.Id, "edit", map[string]any{"path": m.Path, "text": name + " saved " + m.Path + "."}, 0, sc.UserID); err != nil {
			return nil, authz.Internal(err)
		}
		authz.AfterCommit(ctx, func() { s.changed(m.Id) })
	}
	return connect.NewResponse(&v1.SaveFileResponse{Outcome: outcome, Content: string(r.GetData())}), nil
}

func (s *Service) PlanCheckout(ctx context.Context, req *connect.Request[v1.PlanCheckoutRequest]) (*connect.Response[v1.PlanCheckoutResponse], error) {
	sc := authz.ScopeFrom(ctx)
	if err := s.editorReady(ctx, sc.Tx, req.Msg.Id, sc.OrgID); err != nil {
		return nil, connectErr(err)
	}
	r, err := s.callTool(ctx, req.Msg.Id, "pack", "")
	switch {
	case err != nil:
		return nil, connect.NewError(connect.CodeUnavailable, err)
	case r.GetError():
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(r.GetOutput()))
	case len(r.GetData()) > maxSource:
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("the packed checkout is %d MiB; a plan takes at most %d MiB",
			len(r.GetData())>>20, maxSource>>20))
	}
	number, err := s.Hooks.QueuePlan(ctx, sc.OrgID, req.Msg.Id, sc.UserID, r.GetData())
	if err != nil {
		return nil, authz.Internal(err)
	}
	sc.Target(fmt.Sprintf("plan #%d of agent session %s", number, req.Msg.Id))
	return connect.NewResponse(&v1.PlanCheckoutResponse{Number: number}), nil
}

func (s *Service) OpenLanguageServer(ctx context.Context, req *connect.Request[v1.OpenLanguageServerRequest]) (*connect.Response[v1.OpenLanguageServerResponse], error) {
	sc := authz.ScopeFrom(ctx)
	if err := s.editorReady(ctx, sc.Tx, req.Msg.Id, sc.OrgID); err != nil {
		return nil, connectErr(err)
	}
	sc.Target("language server of agent session " + req.Msg.Id)
	t := auth.RandomToken()
	s.mu.Lock()
	if s.tickets == nil {
		s.tickets = map[string]ticket{}
	}
	for k, v := range s.tickets {
		if time.Now().After(v.expires) {
			delete(s.tickets, k)
		}
	}
	s.tickets[t] = ticket{session: req.Msg.Id, org: sc.OrgID, expires: time.Now().Add(ticketLife)}
	s.mu.Unlock()
	return connect.NewResponse(&v1.OpenLanguageServerResponse{Ticket: t}), nil
}

// LanguageServer serves /lsp/<session>: a WebSocket whose first message is
// a ticket from OpenLanguageServer. Each later message is one JSON-RPC
// message, relayed to a tofu-ls in the session's sandbox over the
// toolbox's stream (adr/0034).
func (s *Service) LanguageServer() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session := strings.TrimPrefix(r.URL.Path, "/lsp/")
		// Accept refuses an Origin that is not the request's host.
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.CloseNow() }()
		ws.SetReadLimit(maxFrame)
		ctx := r.Context()
		read := func() ([]byte, error) {
			rctx, cancel := context.WithTimeout(ctx, ticketLife)
			defer cancel()
			_, data, err := ws.Read(rctx)
			return data, err
		}
		first, err := read()
		if err != nil {
			return
		}
		s.mu.Lock()
		t, ok := s.tickets[string(first)]
		delete(s.tickets, string(first))
		s.mu.Unlock()
		if !ok || t.session != session || time.Now().After(t.expires) {
			_ = ws.Close(websocket.StatusPolicyViolation, "a ticket from OpenLanguageServer is required")
			return
		}
		s.relay(ctx, ws, session)
	})
}

func (s *Service) relay(ctx context.Context, ws *websocket.Conn, session string) {
	conn := uuid.NewString()
	out := make(chan []byte, 256)
	l := s.session(session)
	s.mu.Lock()
	if l.lsp == nil {
		l.lsp = map[string]chan []byte{}
	}
	l.lsp[conn] = out
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() {
		s.mu.Lock()
		delete(l.lsp, conn)
		s.mu.Unlock()
		s.sendFrame(context.Background(), session, &v1.LspFrame{Conn: conn, Close: true})
	}()
	go func() {
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case data, ok := <-out:
				if !ok {
					_ = ws.Close(websocket.StatusGoingAway, "the language server stopped")
					return
				}
				if err := ws.Write(ctx, websocket.MessageText, data); err != nil {
					return
				}
			}
		}
	}()
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		if !s.sendFrame(ctx, session, &v1.LspFrame{Conn: conn, Data: data}) {
			return
		}
	}
}

// sendFrame hands a frame to the session's toolbox stream. It reports
// false when no toolbox takes it.
func (s *Service) sendFrame(ctx context.Context, session string, f *v1.LspFrame) bool {
	l := s.session(session)
	s.mu.Lock()
	ready, orders := l.ready, l.orders
	s.mu.Unlock()
	select {
	case <-ready:
	default:
		return false
	}
	select {
	case orders <- &v1.ToolboxOrder{Body: &v1.ToolboxOrder_Lsp{Lsp: f}}:
		return true
	case <-ctx.Done():
		return false
	case <-time.After(toolTimeout):
		return false
	}
}

// lspFromToolbox passes a frame from a tofu-ls to its WebSocket. A frame
// for a WebSocket that is gone is dropped.
func (s *Service) lspFromToolbox(session string, f *v1.LspFrame) {
	l := s.session(session)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := l.lsp[f.GetConn()]
	if out == nil {
		return
	}
	if f.GetClose() {
		close(out)
		delete(l.lsp, f.GetConn())
		return
	}
	select {
	case out <- f.GetData():
	default:
		slog.Warn("language server frame dropped; the browser reads too slowly", "session", session)
	}
}
