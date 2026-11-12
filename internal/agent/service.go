package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/internal/auth"
	"github.com/stackwright/stackwright/internal/authz"
	"github.com/stackwright/stackwright/internal/builders"
	"github.com/stackwright/stackwright/internal/seal"
)

// Errors that callers tell apart.
var (
	ErrNoKey       = errors.New("the org has no OpenRouter key")
	ErrCap         = errors.New("the org's agent spending cap is reached")
	ErrNotLinked   = errors.New("the project has no linked repository")
	ErrBusy        = errors.New("the session is working on a turn")
	ErrDirty       = errors.New("the session has changes that nobody committed or discarded")
	ErrNoSandbox   = errors.New("no build server could start the session's sandbox")
	ErrNoToolbox   = errors.New("the session's sandbox is not connected")
	ErrNoProposal  = errors.New("the session has no proposal waiting with that number")
	ErrNotWaiting  = errors.New("the session waits for no approval")
	errTurnTime    = errors.New("the turn ran out of time")
	sandboxPrefix  = "agent-"
	readyTimeout   = 5 * time.Minute
	adoptWait      = 2 * time.Minute
	toolTimeout    = 6 * time.Minute
	watchFallback  = time.Second
	stepBatch      = 500
	maxModelOutput = 50 << 10
)

// Service runs agent sessions. A session's toolbox stream and its turn live
// in this process, as the hub's builders do.
type Service struct {
	Pool  *pgxpool.Pool
	KEK   seal.KEK
	Hub   *builders.Hub
	Model Model
	Hooks Hooks
	// IdleAfter is how long a sandbox with no changes stays up with no turn.
	IdleAfter time.Duration
	// AdoptWait is how long a restarted server waits for the toolboxes of
	// the sandboxes it adopted to reconnect. Zero means two minutes.
	AdoptWait time.Duration
	// HiddenTools are left out of the tools that the model is offered. The
	// eval uses it to compare runs with and without a tool.
	HiddenTools []string

	mu   sync.Mutex
	live map[string]*live
}

// Hooks reach jobs, runs, state and GitHub. The agent package imports none
// of their packages, so nothing in it can create an apply (adr/0031). The
// server sets them.
type Hooks struct {
	EngineDownload func(ctx context.Context, eng, version, platform string, remote bool) (*v1.Download, error)
	// Plan queues a plan-only agent run of source, a .tar.gz of the
	// checkout, waits for it, and returns a report for the model.
	Plan func(ctx context.Context, orgID, session, userID string, source []byte) (string, error)
	// PlanDetails reports one resource's changes in a plan of the session's
	// workspace, masked. Run 0 is the session's newest plan.
	PlanDetails func(ctx context.Context, orgID, session string, run int32, address string) (string, error)
	// StateLookup reports one resource in the state that the workspace's
	// newest plan read, masked.
	StateLookup func(ctx context.Context, orgID, session, address string) (string, error)
	// OpenPullRequest opens the session's branch against its base, and
	// returns the pull request's number and web address.
	OpenPullRequest func(ctx context.Context, orgID, session, title, body string) (int, string, error)
}

// live is a session's in-process state.
type live struct {
	orgID   string
	gitBase string
	// platform and remote describe the build server that runs the sandbox.
	platform string
	remote   bool
	// job is the running sandbox's hub job id, which is new for each
	// sandbox, so the exit of an older one cannot end it.
	job string
	// adopted marks a sandbox that an earlier server process started.
	adopted bool
	// conn counts toolbox streams, so an old stream that ends late does not
	// undo a newer one.
	conn  int
	ready chan struct{}
	// gone closes when the sandbox exits or is removed, so a turn that
	// waits for its toolbox ends.
	gone    chan struct{}
	orders  chan *v1.ToolboxOrder
	results map[string]chan *v1.ToolResult
	turn    context.CancelFunc
	stopper string
	watch   map[chan struct{}]bool
}

func (s *Service) session(id string) *live {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live == nil {
		s.live = map[string]*live{}
	}
	l, ok := s.live[id]
	if !ok {
		l = &live{ready: make(chan struct{}), gone: make(chan struct{}), orders: make(chan *v1.ToolboxOrder, 4), results: map[string]chan *v1.ToolResult{},
			watch: map[chan struct{}]bool{}}
		s.live[id] = l
	}
	return l
}

func (s *Service) changed(id string) {
	l := s.session(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range l.watch {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (s *Service) inOrg(ctx context.Context, orgID string, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('stackwright.org_id', $1, true)`, orgID); err != nil {
			return err
		}
		return fn(tx)
	})
}

func connectErr(err error) error {
	var ce *connect.Error
	switch {
	case errors.As(err, &ce):
		return err
	case errors.Is(err, ErrNoKey), errors.Is(err, ErrCap), errors.Is(err, ErrNotLinked), errors.Is(err, ErrBusy),
		errors.Is(err, ErrDirty), errors.Is(err, ErrNotWaiting):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, ErrNoProposal), errors.Is(err, pgx.ErrNoRows):
		return connect.NewError(connect.CodeNotFound, errors.New("no such session or proposal"))
	case errors.Is(err, ErrNoSandbox):
		return connect.NewError(connect.CodeUnavailable, err)
	}
	return authz.Internal(err)
}

// settings

type settings struct {
	key           string
	turn, short   string
	cap, spent    float64
	hasKey        bool
	sealedKey     []byte
	updatedByName string
}

func (s *Service) loadSettings(ctx context.Context, tx pgx.Tx, orgID string, withKey bool) (settings, error) {
	st := settings{turn: "anthropic/claude-opus-5.5", short: "anthropic/claude-haiku-5.5", cap: 50}
	err := tx.QueryRow(ctx, `SELECT api_key_sealed, turn_model, short_model, monthly_cap::float8 FROM agent_settings WHERE org_id = $1`, orgID).
		Scan(&st.sealedKey, &st.turn, &st.short, &st.cap)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return st, err
	}
	st.hasKey = len(st.sealedKey) > 0
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(cost), 0)::float8 FROM agent_steps
		WHERE org_id = $1 AND at >= date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'`, orgID).Scan(&st.spent); err != nil {
		return st, err
	}
	if withKey && st.hasKey {
		plain, err := s.KEK.Open(ctx, tx, orgID, st.sealedKey)
		if err != nil {
			return st, err
		}
		st.key = string(plain)
	}
	return st, nil
}

func (st settings) proto() *v1.AgentSettings {
	return &v1.AgentSettings{HasKey: st.hasKey, TurnModel: st.turn, ShortModel: st.short, MonthlyCap: st.cap, SpentThisMonth: st.spent}
}

func (s *Service) GetAgentSettings(ctx context.Context, req *connect.Request[v1.GetAgentSettingsRequest]) (*connect.Response[v1.GetAgentSettingsResponse], error) {
	sc := authz.ScopeFrom(ctx)
	st, err := s.loadSettings(ctx, sc.Tx, sc.OrgID, false)
	if err != nil {
		return nil, authz.Internal(err)
	}
	return connect.NewResponse(&v1.GetAgentSettingsResponse{Settings: st.proto()}), nil
}

func (s *Service) SetAgentSettings(ctx context.Context, req *connect.Request[v1.SetAgentSettingsRequest]) (*connect.Response[v1.SetAgentSettingsResponse], error) {
	sc := authz.ScopeFrom(ctx)
	m := req.Msg
	if m.MonthlyCap < 0 || m.MonthlyCap > 100000 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("the cap must be between $0 and $100,000"))
	}
	for _, model := range []string{m.TurnModel, m.ShortModel} {
		if model == "" || len(model) > 200 || strings.ContainsAny(model, " \t\n") {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("each model needs an OpenRouter model id, such as anthropic/claude-opus-5.5"))
		}
	}
	var sealed []byte
	if m.ApiKey != "" {
		var err error
		if sealed, err = s.KEK.Seal(ctx, sc.Tx, sc.OrgID, []byte(strings.TrimSpace(m.ApiKey))); err != nil {
			return nil, authz.Internal(err)
		}
	}
	_, err := sc.Tx.Exec(ctx, `INSERT INTO agent_settings (org_id, api_key_sealed, turn_model, short_model, monthly_cap, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (org_id) DO UPDATE SET turn_model = $3, short_model = $4, monthly_cap = $5, updated_by = $6, updated_at = now(),
			api_key_sealed = CASE WHEN $7 THEN NULL WHEN $2::bytea IS NULL THEN agent_settings.api_key_sealed ELSE $2 END`,
		sc.OrgID, sealed, m.TurnModel, m.ShortModel, m.MonthlyCap, sc.UserID, m.ClearKey)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("the settings were not saved: %w", err))
	}
	key := "kept"
	switch {
	case m.ClearKey:
		key = "removed"
	case m.ApiKey != "":
		key = "replaced"
	}
	sc.Target(fmt.Sprintf("agent settings turn=%s short=%s cap=%.2f key=%s", m.TurnModel, m.ShortModel, m.MonthlyCap, key))
	st, err := s.loadSettings(ctx, sc.Tx, sc.OrgID, false)
	if err != nil {
		return nil, authz.Internal(err)
	}
	return connect.NewResponse(&v1.SetAgentSettingsResponse{Settings: st.proto()}), nil
}

// sessions

const sessionColumns = `s.id, p.slug, w.name, s.branch, s.status, s.title, coalesce(nullif(u.name, ''), u.email, ''),
	s.created_at, s.updated_at, s.dirty,
	(SELECT coalesce(sum(cost), 0)::float8 FROM agent_steps st WHERE st.session_id = s.id), coalesce(s.error, ''), s.last_seq,
	coalesce(s.pr_number, 0), coalesce(s.pr_url, '')
	FROM agent_sessions s JOIN workspaces w ON w.id = s.workspace_id JOIN projects p ON p.id = w.project_id
	LEFT JOIN users u ON u.id = s.created_by`

func scanSession(row pgx.CollectableRow) (*v1.AgentSession, error) {
	a := &v1.AgentSession{}
	var status string
	var created, updated time.Time
	err := row.Scan(&a.Id, &a.Project, &a.Workspace, &a.Branch, &status, &a.Title, &a.CreatedBy, &created, &updated, &a.Dirty,
		&a.Spent, &a.Error, &a.LastSeq, &a.PrNumber, &a.PrUrl)
	a.Status = v1.AgentSessionStatus(v1.AgentSessionStatus_value["AGENT_SESSION_STATUS_"+strings.ToUpper(status)])
	a.CreatedAt, a.UpdatedAt = timestamppb.New(created), timestamppb.New(updated)
	return a, err
}

func oneSession(ctx context.Context, tx pgx.Tx, id string) (*v1.AgentSession, error) {
	rows, err := tx.Query(ctx, `SELECT `+sessionColumns+` WHERE s.id = $1`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectExactlyOneRow(rows, scanSession)
}

func (s *Service) ListSessions(ctx context.Context, req *connect.Request[v1.ListSessionsRequest]) (*connect.Response[v1.ListSessionsResponse], error) {
	tx := authz.ScopeFrom(ctx).Tx
	rows, err := tx.Query(ctx, `SELECT `+sessionColumns+` WHERE p.slug = $1 AND w.name = $2 AND s.status <> 'closed'
		ORDER BY s.created_at DESC LIMIT 50`, req.Msg.Project, req.Msg.Workspace)
	if err != nil {
		return nil, authz.Internal(err)
	}
	list, err := pgx.CollectRows(rows, scanSession)
	if err != nil {
		return nil, authz.Internal(err)
	}
	return connect.NewResponse(&v1.ListSessionsResponse{Sessions: list}), nil
}

func (s *Service) StartSession(ctx context.Context, req *connect.Request[v1.StartSessionRequest]) (*connect.Response[v1.StartSessionResponse], error) {
	sc := authz.ScopeFrom(ctx)
	m := req.Msg
	text := strings.TrimSpace(m.Message)
	if text == "" || len(text) > 20000 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("a message needs 1 to 20,000 characters"))
	}
	st, err := s.loadSettings(ctx, sc.Tx, sc.OrgID, false)
	if err != nil {
		return nil, authz.Internal(err)
	}
	if err := allowed(st); err != nil {
		return nil, connectErr(err)
	}
	var workspaceID, repoID, base string
	err = sc.Tx.QueryRow(ctx, `SELECT w.id, rp.id, coalesce(nullif(p.branch, ''), rp.default_branch)
		FROM workspaces w JOIN projects p ON p.id = w.project_id
		JOIN repos rp ON rp.org_id = p.org_id AND lower(rp.full_name) = lower(p.repo)
		WHERE p.slug = $1 AND w.name = $2 AND p.archived_at IS NULL AND w.archived_at IS NULL`, m.Project, m.Workspace).
		Scan(&workspaceID, &repoID, &base)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, connectErr(ErrNotLinked)
	}
	if err != nil {
		return nil, authz.Internal(err)
	}
	id := uuid.NewString()
	branch := "agent/" + slugOf(text) + "-" + id[:4]
	if _, err := sc.Tx.Exec(ctx, `INSERT INTO agent_sessions (id, org_id, workspace_id, repo_id, branch, base, status, title, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, 'starting', $7, $8)`, id, sc.OrgID, workspaceID, repoID, branch, base, titleOf(text), sc.UserID); err != nil {
		return nil, authz.Internal(err)
	}
	if err := appendStep(ctx, sc.Tx, sc.OrgID, id, "user", map[string]any{"text": text}, 0, sc.UserID); err != nil {
		return nil, authz.Internal(err)
	}
	session, err := oneSession(ctx, sc.Tx, id)
	if err != nil {
		return nil, authz.Internal(err)
	}
	sc.Target(fmt.Sprintf("agent session %s on %s/%s branch %s", id, m.Project, m.Workspace, branch))
	orgID, userID := sc.OrgID, sc.UserID
	authz.AfterCommit(ctx, func() { s.startTurn(id, orgID, userID) })
	return connect.NewResponse(&v1.StartSessionResponse{Session: session}), nil
}

func allowed(st settings) error {
	if !st.hasKey {
		return ErrNoKey
	}
	if st.spent >= st.cap {
		return fmt.Errorf("%w: $%.2f of $%.2f spent this month; an admin can raise the cap in Settings › Agent", ErrCap, st.spent, st.cap)
	}
	return nil
}

func (s *Service) SendMessage(ctx context.Context, req *connect.Request[v1.SendMessageRequest]) (*connect.Response[v1.SendMessageResponse], error) {
	sc := authz.ScopeFrom(ctx)
	m := req.Msg
	text := strings.TrimSpace(m.Message)
	if text == "" || len(text) > 20000 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("a message needs 1 to 20,000 characters"))
	}
	st, err := s.loadSettings(ctx, sc.Tx, sc.OrgID, false)
	if err != nil {
		return nil, authz.Internal(err)
	}
	if err := allowed(st); err != nil {
		return nil, connectErr(err)
	}
	var status string
	if err := sc.Tx.QueryRow(ctx, `SELECT status FROM agent_sessions WHERE id = $1 FOR UPDATE`, m.Id).Scan(&status); err != nil {
		return nil, connectErr(err)
	}
	switch status {
	case "working", "starting":
		return nil, connectErr(ErrBusy)
	case "closed":
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the session is closed"))
	}
	if status == "waiting_for_answer" {
		// The answer is the result of the ask_user call that waits.
		var call string
		if err := sc.Tx.QueryRow(ctx, `SELECT payload->>'id' FROM agent_steps WHERE session_id = $1 AND kind = 'tool_call'
			ORDER BY seq DESC LIMIT 1`, m.Id).Scan(&call); err != nil {
			return nil, authz.Internal(err)
		}
		err = appendStep(ctx, sc.Tx, sc.OrgID, m.Id, "tool_result", map[string]any{"id": call, "output": text, "answer": true}, 0, sc.UserID)
	} else {
		err = appendStep(ctx, sc.Tx, sc.OrgID, m.Id, "user", map[string]any{"text": text}, 0, sc.UserID)
	}
	if err != nil {
		return nil, authz.Internal(err)
	}
	if _, err := sc.Tx.Exec(ctx, `UPDATE agent_sessions SET status = 'working', error = NULL, updated_at = now() WHERE id = $1`, m.Id); err != nil {
		return nil, authz.Internal(err)
	}
	session, err := oneSession(ctx, sc.Tx, m.Id)
	if err != nil {
		return nil, authz.Internal(err)
	}
	sc.Target("agent message in session " + m.Id)
	orgID, userID := sc.OrgID, sc.UserID
	authz.AfterCommit(ctx, func() { s.startTurn(m.Id, orgID, userID) })
	return connect.NewResponse(&v1.SendMessageResponse{Session: session}), nil
}

func (s *Service) StopTurn(ctx context.Context, req *connect.Request[v1.StopTurnRequest]) (*connect.Response[v1.StopTurnResponse], error) {
	sc := authz.ScopeFrom(ctx)
	var name string
	if err := sc.Tx.QueryRow(ctx, `SELECT coalesce(nullif(u.name, ''), u.email) FROM agent_sessions s, users u
		WHERE s.id = $1 AND u.id = $2`, req.Msg.Id, sc.UserID).Scan(&name); err != nil {
		return nil, connectErr(err)
	}
	l := s.session(req.Msg.Id)
	s.mu.Lock()
	cancel := l.turn
	l.stopper = name
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	sc.Target("agent stop in session " + req.Msg.Id)
	return connect.NewResponse(&v1.StopTurnResponse{}), nil
}

func (s *Service) DecideProposal(ctx context.Context, req *connect.Request[v1.DecideProposalRequest]) (*connect.Response[v1.DecideProposalResponse], error) {
	sc := authz.ScopeFrom(ctx)
	m := req.Msg
	var status string
	var p proposal
	err := sc.Tx.QueryRow(ctx, `SELECT s.status, coalesce(st.payload->>'type', 'commit'), coalesce(st.payload->>'message', ''),
		coalesce(st.payload->>'title', ''), coalesce(st.payload->>'body', '') FROM agent_sessions s
		JOIN agent_steps st ON st.session_id = s.id AND st.seq = $2 AND st.kind = 'proposal'
		WHERE s.id = $1 FOR UPDATE OF s`, m.Id, m.Proposal).Scan(&status, &p.kind, &p.message, &p.title, &p.body)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, connectErr(ErrNoProposal)
	}
	if err != nil {
		return nil, authz.Internal(err)
	}
	if status != "waiting_for_approval" {
		return nil, connectErr(ErrNotWaiting)
	}
	var name, email string
	if err := sc.Tx.QueryRow(ctx, `SELECT coalesce(nullif(name, ''), email), email FROM users WHERE id = $1`, sc.UserID).Scan(&name, &email); err != nil {
		return nil, authz.Internal(err)
	}
	if m.Approve && p.kind == "commit" {
		// The Git proxy lets one push through for this row (adr/0030).
		if _, err := sc.Tx.Exec(ctx, `INSERT INTO agent_approvals (session_id, org_id, proposal_seq, approved_by) VALUES ($1, $2, $3, $4)
			ON CONFLICT DO NOTHING`, m.Id, sc.OrgID, m.Proposal, sc.UserID); err != nil {
			return nil, authz.Internal(err)
		}
	}
	if _, err := sc.Tx.Exec(ctx, `UPDATE agent_sessions SET status = 'working', updated_at = now() WHERE id = $1`, m.Id); err != nil {
		return nil, authz.Internal(err)
	}
	session, err := oneSession(ctx, sc.Tx, m.Id)
	if err != nil {
		return nil, authz.Internal(err)
	}
	verb := "discard"
	switch {
	case m.Approve && p.kind == "pull_request":
		verb = "open pull request"
	case m.Approve:
		verb = "commit and push"
	}
	sc.Target(fmt.Sprintf("agent proposal %d in session %s: %s", m.Proposal, m.Id, verb))
	orgID, userID := sc.OrgID, sc.UserID
	authz.AfterCommit(ctx, func() {
		go s.carryOut(m.Id, orgID, userID, m.Proposal, m.Approve, p, name, email)
	})
	return connect.NewResponse(&v1.DecideProposalResponse{Session: session}), nil
}

func (s *Service) CloseSession(ctx context.Context, req *connect.Request[v1.CloseSessionRequest]) (*connect.Response[v1.CloseSessionResponse], error) {
	sc := authz.ScopeFrom(ctx)
	var dirty bool
	var status string
	if err := sc.Tx.QueryRow(ctx, `SELECT dirty, status FROM agent_sessions WHERE id = $1 FOR UPDATE`, req.Msg.Id).Scan(&dirty, &status); err != nil {
		return nil, connectErr(err)
	}
	if status == "working" || status == "starting" {
		return nil, connectErr(ErrBusy)
	}
	if dirty && !req.Msg.Discard {
		return nil, connectErr(ErrDirty)
	}
	if _, err := sc.Tx.Exec(ctx, `UPDATE agent_sessions SET status = 'closed', updated_at = now() WHERE id = $1`, req.Msg.Id); err != nil {
		return nil, authz.Internal(err)
	}
	sc.Target(fmt.Sprintf("agent session %s closed discard=%t", req.Msg.Id, req.Msg.Discard))
	orgID := sc.OrgID
	authz.AfterCommit(ctx, func() { s.removeSandbox(context.Background(), req.Msg.Id, orgID, "closed") })
	return connect.NewResponse(&v1.CloseSessionResponse{}), nil
}

const stepQuery = `SELECT st.seq, st.kind, st.payload::text, st.at, coalesce(nullif(u.name, ''), u.email, '')
	FROM agent_steps st LEFT JOIN users u ON u.id = st.actor_id WHERE st.session_id = $1 AND st.seq > $2 ORDER BY st.seq LIMIT $3`

func (s *Service) WatchSession(ctx context.Context, req *connect.Request[v1.WatchSessionRequest], stream *connect.ServerStream[v1.WatchSessionResponse]) error {
	sc := authz.ScopeFrom(ctx)
	l := s.session(req.Msg.Id)
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	l.watch[ch] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(l.watch, ch)
		s.mu.Unlock()
	}()
	tick := time.NewTicker(watchFallback)
	defer tick.Stop()
	cursor := req.Msg.AfterSeq
	var sent time.Time
	for {
		var session *v1.AgentSession
		var steps []*v1.AgentStep
		err := sc.Read(ctx, s.Pool, func(tx pgx.Tx) error {
			var err error
			if session, err = oneSession(ctx, tx, req.Msg.Id); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, stepQuery, req.Msg.Id, cursor, stepBatch)
			if err != nil {
				return err
			}
			steps, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (*v1.AgentStep, error) {
				st := &v1.AgentStep{}
				var at time.Time
				err := r.Scan(&st.Seq, &st.Kind, &st.Payload, &at, &st.Actor)
				st.At = timestamppb.New(at)
				return st, err
			})
			return err
		})
		if err != nil {
			return connectErr(err)
		}
		resp := &v1.WatchSessionResponse{Steps: steps}
		if updated := session.UpdatedAt.AsTime(); !updated.Equal(sent) {
			resp.Session, sent = session, updated
		}
		if len(steps) > 0 || resp.Session != nil {
			if err := stream.Send(resp); err != nil {
				return err
			}
		}
		if len(steps) > 0 {
			cursor = steps[len(steps)-1].Seq
			if len(steps) == stepBatch {
				continue
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ch:
		case <-tick.C:
		}
	}
}

// appendStep adds a step to the session's transcript and moves its last_seq.
func appendStep(ctx context.Context, tx pgx.Tx, orgID, session, kind string, payload map[string]any, cost float64, actor string) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var seq int64
	if err := tx.QueryRow(ctx, `UPDATE agent_sessions SET last_seq = last_seq + 1, updated_at = now(), idle_since = now()
		WHERE id = $1 RETURNING last_seq`, session).Scan(&seq); err != nil {
		return err
	}
	var actorID *string
	if actor != "" {
		actorID = &actor
	}
	_, err = tx.Exec(ctx, `INSERT INTO agent_steps (session_id, org_id, seq, kind, payload, cost, actor_id) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		session, orgID, seq, kind, b, cost, actorID)
	return err
}

// step appends a step outside a request, and tells watchers.
func (s *Service) step(ctx context.Context, orgID, session, kind string, payload map[string]any, cost float64, actor string) error {
	err := s.inOrg(ctx, orgID, func(tx pgx.Tx) error {
		return appendStep(ctx, tx, orgID, session, kind, payload, cost, actor)
	})
	s.changed(session)
	return err
}

func (s *Service) setStatus(ctx context.Context, orgID, session, status, msg string) {
	err := s.inOrg(ctx, orgID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agent_sessions SET status = $2, error = nullif($3, ''), updated_at = now(), idle_since = now()
			WHERE id = $1 AND status <> 'closed'`, session, status, msg)
		return err
	})
	if err != nil {
		slog.Error("agent session status", "session", session, "err", err)
	}
	s.changed(session)
}

func (s *Service) setDirty(ctx context.Context, orgID, session string, dirty bool) {
	err := s.inOrg(ctx, orgID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agent_sessions SET dirty = $2, updated_at = now() WHERE id = $1`, session, dirty)
		return err
	})
	if err != nil {
		slog.Error("agent session dirty", "session", session, "err", err)
	}
}

// the sandbox

// ensureSandbox starts the session's sandbox if none runs, and waits until
// its toolbox has the checkout.
func (s *Service) ensureSandbox(ctx context.Context, session, orgID string) error {
	l := s.session(session)
	select {
	case <-l.ready:
		return nil
	default:
	}
	var labels map[string]string
	var running *string
	err := s.inOrg(ctx, orgID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT w.required_labels, s.sandbox_id FROM agent_sessions s JOIN workspaces w ON w.id = s.workspace_id
			WHERE s.id = $1`, session).Scan(&labels, &running)
	})
	if err != nil {
		return err
	}
	if running == nil {
		slot, ok := s.Hub.ReserveWith(orgID, labels, "", builders.Toolbox)
		if !ok {
			return fmt.Errorf("%w: none with a free slot runs agent sandboxes; a build server needs its builder updated to run them", ErrNoSandbox)
		}
		token := auth.RandomToken()
		job := sandboxPrefix + session + "-" + auth.RandomToken()[:8]
		gitBase := s.Hub.ServerURL
		if !slot.Remote {
			gitBase = s.Hub.BuiltInServerURL
		}
		err := s.inOrg(ctx, orgID, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO agent_sandbox_tokens (token_hash, session_id, org_id) VALUES ($1, $2, $3)`,
				auth.HashToken(token), session, orgID); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE agent_sessions SET sandbox_id = $2, sandbox_platform = $3, sandbox_remote = $4, status = 'starting',
				updated_at = now() WHERE id = $1`, session, job, slot.Platform, slot.Remote)
			return err
		})
		if err != nil {
			slot.Release()
			return err
		}
		s.mu.Lock()
		l.orgID, l.gitBase, l.platform, l.remote, l.job, l.adopted = orgID, gitBase, slot.Platform, slot.Remote, job, false
		l.gone = make(chan struct{})
		s.mu.Unlock()
		s.changed(session)
		if err := slot.Start(&v1.StartSandbox{JobId: job, Ticket: token, Toolbox: true}); err != nil {
			return fmt.Errorf("%w: %w", ErrNoSandbox, err)
		}
	}
	s.mu.Lock()
	ready, gone, adopted := l.ready, l.gone, l.adopted
	s.mu.Unlock()
	select {
	case <-ready:
		return nil
	case <-gone:
		// An adopted sandbox whose toolbox never came back is removed;
		// start a fresh one.
		if adopted {
			return s.ensureSandbox(ctx, session, orgID)
		}
		return fmt.Errorf("%w: the sandbox exited before its toolbox connected", ErrNoSandbox)
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(readyTimeout):
		return fmt.Errorf("%w: its toolbox did not report within %s", ErrNoSandbox, readyTimeout)
	}
}
