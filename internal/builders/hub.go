// Package builders enrols build servers, signs their certificates, keeps
// their streams, and places sandboxes on them (adr/0007, adr/0021).
package builders

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"sync"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/internal/auth"
	"github.com/stackwright/stackwright/internal/authz"
)

var (
	ErrGone       = errors.New("the build server disconnected")
	errOffline    = errors.New("the build server is not connected")
	validPlatform = regexp.MustCompile(`^linux_(amd64|arm64)$`)
	validName     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	validLabelKey = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,62}$`)
	validLabelVal = regexp.MustCompile(`^[A-Za-z0-9._-]{0,63}$`)
	semver        = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)`)
)

const BuiltInName = "built-in"

// Hub serves BuilderService and keeps the connected builders in memory, so
// one server process holds them all (adr/0020).
type Hub struct {
	Pool *pgxpool.Pool
	CA   *CA
	// BuiltInToken enrols the built-in builder, which serves every org.
	BuiltInToken string
	// BuilderURL is where remote builders reach the builder port.
	BuilderURL string
	Version    string
	// Images holds the executor image for each sandbox backend.
	Images map[string]string
	// ServerURL is the server as sandboxes of remote builders reach it;
	// BuiltInServerURL as those of the built-in builder reach it.
	ServerURL        string
	BuiltInServerURL string
	// OnExit is called when a job's sandbox exits or cannot start.
	OnExit func(jobID string, s *v1.SandboxStatus)

	mu       sync.Mutex
	builders map[string]*builder
	changed  chan struct{}
}

type builder struct {
	id       string
	orgID    string
	name     string
	platform string
	backend  string
	upgrade  string
	features []string
	labels   map[string]string
	slots    int
	draining bool
	reserved int
	jobs     map[string]bool
	tests    map[string]chan *v1.SandboxStatus
	send     chan *v1.ConnectResponse
	done     chan struct{}
	close    context.CancelFunc
}

func (b *builder) used() int { return b.reserved + len(b.jobs) }

// Slot is a place on a builder, held until Start or Release.
type Slot struct {
	h        *Hub
	b        *builder
	ID       string
	Builder  string
	Platform string
	// Remote is false for the built-in builder.
	Remote bool
}

// Toolbox is the feature of a builder that runs agent sandboxes.
const Toolbox = "toolbox"

// Reserve holds a free slot on the least busy builder that serves orgID,
// has every required label, is not draining, and runs platform if it is
// set.
func (h *Hub) Reserve(orgID string, required map[string]string, platform string) (*Slot, bool) {
	return h.ReserveWith(orgID, required, platform, "")
}

// ReserveWith is Reserve for builders that also list feature, unless it is
// empty.
func (h *Hub) ReserveWith(orgID string, required map[string]string, platform, feature string) (*Slot, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var best *builder
	for _, b := range h.builders {
		if b.draining || b.used() >= b.slots || (b.orgID != "" && b.orgID != orgID) || (platform != "" && b.platform != platform) {
			continue
		}
		if feature != "" && !slices.Contains(b.features, feature) {
			continue
		}
		if !hasLabels(b.labels, required) {
			continue
		}
		if best == nil || b.used() < best.used() {
			best = b
		}
	}
	if best == nil {
		return nil, false
	}
	best.reserved++
	return &Slot{h: h, b: best, ID: best.id, Builder: best.name, Platform: best.platform, Remote: best.orgID != ""}, true
}

func hasLabels(have, want map[string]string) bool {
	for k, v := range want {
		if got, ok := have[k]; !ok || got != v {
			return false
		}
	}
	return true
}

func (s *Slot) Release() {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	s.b.reserved--
}

// Start turns the slot into a running job and sends the order, with the
// image and server URL for this builder.
func (s *Slot) Start(order *v1.StartSandbox) error {
	s.h.mu.Lock()
	s.b.reserved--
	s.b.jobs[order.GetJobId()] = true
	s.h.mu.Unlock()
	order.Image = s.h.Images[s.b.backend]
	order.ServerUrl = s.h.ServerURL
	if !s.Remote {
		order.ServerUrl = s.h.BuiltInServerURL
	}
	return s.h.send(s.b, &v1.ConnectResponse{Body: &v1.ConnectResponse_Start{Start: order}})
}

// Stop asks the builder that runs jobID to stop its sandbox. A job on no
// connected builder is left to the reaper.
func (h *Hub) Stop(jobID string) {
	h.mu.Lock()
	var on *builder
	for _, b := range h.builders {
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

// Live is what the hub knows of a connected build server.
type Live struct {
	Running int
	Upgrade string
}

// Status returns the connected build servers by id.
func (h *Hub) Status() map[string]Live {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]Live, len(h.builders))
	for id, b := range h.builders {
		out[id] = Live{Running: len(b.jobs), Upgrade: b.upgrade}
	}
	return out
}

// Update changes a connected build server's settings. Nil leaves one as it
// is.
func (h *Hub) Update(id string, labels map[string]string, slots *int, draining *bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.builders[id]
	if b == nil {
		return
	}
	if labels != nil {
		b.labels = labels
	}
	if slots != nil {
		b.slots = *slots
	}
	if draining != nil {
		b.draining = *draining
	}
}

// Disconnect closes a build server's stream. Its certificates must already
// be revoked, or it connects again.
func (h *Hub) Disconnect(id string) {
	h.mu.Lock()
	b := h.builders[id]
	h.mu.Unlock()
	if b != nil {
		b.close()
	}
}

// notify wakes WaitOnline. h.mu must be held.
func (h *Hub) notify() {
	if h.changed != nil {
		close(h.changed)
	}
	h.changed = make(chan struct{})
}

// WaitOnline waits until the build server connects.
func (h *Hub) WaitOnline(ctx context.Context, id string) error {
	for {
		h.mu.Lock()
		_, ok := h.builders[id]
		if h.changed == nil {
			h.changed = make(chan struct{})
		}
		changed := h.changed
		h.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %w", errOffline, ctx.Err())
		case <-changed:
		}
	}
}

// Test starts a sandbox that runs the executor's version command, and
// returns an error unless it exits with 0.
func (h *Hub) Test(ctx context.Context, id string) error {
	h.mu.Lock()
	b := h.builders[id]
	testID := "test-" + uuid.NewString()
	result := make(chan *v1.SandboxStatus, 1)
	if b != nil {
		b.tests[testID] = result
	}
	h.mu.Unlock()
	if b == nil {
		return errOffline
	}
	defer func() {
		h.mu.Lock()
		delete(b.tests, testID)
		h.mu.Unlock()
	}()
	if err := h.send(b, &v1.ConnectResponse{Body: &v1.ConnectResponse_Start{Start: &v1.StartSandbox{
		JobId: testID, Image: h.Images[b.backend], Test: true,
	}}}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return fmt.Errorf("the test sandbox did not finish: %w", ctx.Err())
	case <-b.done:
		return ErrGone
	case st := <-result:
		if st.GetState() == v1.SandboxStatus_STATE_FAILED {
			return fmt.Errorf("the test sandbox did not start: %s", st.GetError())
		}
		if st.GetExitCode() != 0 {
			return fmt.Errorf("the test sandbox exited with %d", st.GetExitCode())
		}
		return nil
	}
}

// compatible accepts a builder with the server's major version and at most
// two minor versions behind. upgrade is set when the builder is older.
// Builds that are not releases, such as "dev", always pass.
func compatible(server, builder string) (upgrade string, err error) {
	s, b := semver.FindStringSubmatch(server), semver.FindStringSubmatch(builder)
	if s == nil || b == nil {
		return "", nil
	}
	num := func(m []string, i int) int { n, _ := strconv.Atoi(m[i]); return n }
	switch {
	case num(s, 1) != num(b, 1):
		return "", fmt.Errorf("builder %s cannot work with server %s: the major versions differ", builder, server)
	case num(b, 2) < num(s, 2)-2:
		return "", fmt.Errorf("builder %s is more than two minor versions behind server %s; install the new builder", builder, server)
	case num(b, 2) < num(s, 2) || (num(b, 2) == num(s, 2) && num(b, 3) < num(s, 3)):
		return fmt.Sprintf("Builder %s is older than the server (%s). Upgrade it.", builder, server), nil
	}
	return "", nil
}

// CheckLabels refuses labels that a build server or workspace cannot hold.
func CheckLabels(labels map[string]string) error {
	if len(labels) > 20 {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("at most 20 labels"))
	}
	for k, v := range labels {
		if !validLabelKey.MatchString(k) || !validLabelVal.MatchString(v) {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(
				"label %s=%s: a key is lower-case letters, digits, '.', '_', '/' and '-'; a value is letters, digits, '.', '_' and '-'", k, v))
		}
	}
	return nil
}

// CheckServer refuses a name or slot count that a build server cannot have.
func CheckServer(name string, slots int32, labels map[string]string) error {
	if !validName.MatchString(name) {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("a build server name is lower-case letters, digits and hyphens"))
	}
	if slots < 1 || slots > 64 {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("a build server has 1 to 64 slots"))
	}
	return CheckLabels(labels)
}

func isUnique(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}

func (h *Hub) Enrol(ctx context.Context, req *connect.Request[v1.EnrolRequest]) (*connect.Response[v1.EnrolResponse], error) {
	s := authz.ScopeFrom(ctx)
	resp := &v1.EnrolResponse{CaCertificate: h.CA.Certificate(), BuilderUrl: h.BuilderURL}
	var orgID string
	if h.BuiltInToken != "" && subtle.ConstantTimeCompare([]byte(req.Msg.GetToken()), []byte(h.BuiltInToken)) == 1 {
		if err := CheckServer(BuiltInName, req.Msg.GetSlots(), req.Msg.GetLabels()); err != nil {
			return nil, err
		}
		labels := []byte("{}")
		if len(req.Msg.GetLabels()) > 0 {
			labels, _ = json.Marshal(req.Msg.GetLabels())
		}
		err := s.Tx.QueryRow(ctx, `INSERT INTO build_servers (id, name, labels, slots, enrolled_via) VALUES ($1, $2, $3, $4, 'builtin')
			ON CONFLICT (org_id, name) WHERE removed_at IS NULL DO UPDATE SET labels = excluded.labels, slots = excluded.slots
			RETURNING id`, uuid.NewString(), BuiltInName, labels, req.Msg.GetSlots()).Scan(&resp.BuildServerId)
		if err != nil {
			return nil, authz.Internal(err)
		}
		resp.Name = BuiltInName
	} else {
		var serverID, name, via, sshHost, sshUser, hostKey string
		var labels []byte
		var slots, sshPort int
		var createdBy *string
		err := s.Tx.QueryRow(ctx, `UPDATE enrol_tokens SET used_at = now() WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()
			RETURNING build_server_id, org_id, name, labels, slots, via, ssh_host, ssh_port, ssh_user, ssh_host_key, created_by`,
			auth.HashToken(req.Msg.GetToken())).Scan(&serverID, &orgID, &name, &labels, &slots, &via, &sshHost, &sshPort, &sshUser, &hostKey, &createdBy)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("the enrol token is unknown, spent or expired"))
		}
		if err != nil {
			return nil, authz.Internal(err)
		}
		if err := s.SetOrg(ctx, orgID); err != nil {
			return nil, authz.Internal(err)
		}
		_, err = s.Tx.Exec(ctx, `INSERT INTO build_servers (id, org_id, name, labels, slots, enrolled_via, ssh_host, ssh_port, ssh_user, ssh_host_key, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			serverID, orgID, name, labels, slots, via, sshHost, sshPort, sshUser, hostKey, createdBy)
		if isUnique(err) {
			return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("the org already has a build server named %s", name))
		}
		if err != nil {
			return nil, authz.Internal(err)
		}
		resp.BuildServerId, resp.Name = serverID, name
	}
	cert, err := h.CA.issue(ctx, s.Tx, req.Msg.GetCsr(), resp.GetBuildServerId(), orgID)
	if errors.Is(err, errBadCSR) {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err != nil {
		return nil, authz.Internal(err)
	}
	resp.Certificate = cert
	s.Target("build server " + resp.GetName())
	return connect.NewResponse(resp), nil
}

func (h *Hub) Connect(ctx context.Context, stream *connect.BidiStream[v1.ConnectRequest, v1.ConnectResponse]) error {
	scope := authz.ScopeFrom(ctx)
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	switch {
	case hello == nil:
		return connect.NewError(connect.CodeInvalidArgument, errors.New("the first message must be a hello"))
	case !validPlatform.MatchString(hello.GetPlatform()):
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("platform %q is not supported", hello.GetPlatform()))
	case h.Images[hello.GetBackend()] == "":
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("the server has no executor image for sandbox backend %q", hello.GetBackend()))
	}
	upgrade, err := compatible(h.Version, hello.GetVersion())
	if err != nil {
		return connect.NewError(connect.CodeFailedPrecondition, err)
	}

	ctx, cancel := context.WithDeadline(ctx, scope.CertExpires)
	defer cancel()
	b := &builder{
		id: scope.BuildServerID, orgID: scope.OrgID, platform: hello.GetPlatform(), backend: hello.GetBackend(), features: hello.GetFeatures(),
		upgrade: upgrade, jobs: map[string]bool{}, tests: map[string]chan *v1.SandboxStatus{},
		send: make(chan *v1.ConnectResponse, 16), done: make(chan struct{}), close: cancel,
	}
	var labels []byte
	if err := scope.Read(ctx, h.Pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `UPDATE build_servers SET platform = $2, backend = $3, version = $4, last_seen_at = now()
			WHERE id = $1 AND removed_at IS NULL RETURNING name, labels, slots, draining`,
			b.id, b.platform, b.backend, hello.GetVersion()).Scan(&b.name, &labels, &b.slots, &b.draining)
	}); err != nil {
		return authz.Internal(fmt.Errorf("load build server %s: %w", b.id, err))
	}
	if err := json.Unmarshal(labels, &b.labels); err != nil {
		return authz.Internal(err)
	}
	for _, j := range hello.GetRunningJobs() {
		b.jobs[j] = true
	}
	h.mu.Lock()
	if h.builders == nil {
		h.builders = map[string]*builder{}
	}
	if old := h.builders[b.id]; old != nil {
		old.close()
	}
	h.builders[b.id] = b
	h.notify()
	h.mu.Unlock()
	slog.Info("builder connected", "name", b.name, "id", b.id, "platform", b.platform, "version", hello.GetVersion())
	defer func() {
		h.mu.Lock()
		if h.builders[b.id] == b {
			delete(h.builders, b.id)
			h.notify()
		}
		h.mu.Unlock()
		close(b.done)
		slog.Info("builder disconnected", "name", b.name, "id", b.id)
	}()

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
	// Receive blocks until the client sends or the stream ends, so it runs
	// apart from ctx, which Disconnect and the certificate's end cancel.
	received := make(chan *v1.ConnectRequest)
	recvErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Receive()
			if err != nil {
				recvErr <- err
				return
			}
			select {
			case received <- msg:
			case <-ctx.Done():
				return
			}
		}
	}()
	for {
		var msg *v1.ConnectRequest
		select {
		case <-ctx.Done():
			return connect.NewError(connect.CodeUnavailable, errors.New("the stream closed; connect again"))
		case err := <-recvErr:
			select {
			case err = <-sendErr:
			default:
			}
			return err
		case msg = <-received:
		}
		switch {
		case msg.GetHeartbeat() != nil:
			if err := scope.Read(ctx, h.Pool, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE build_servers SET last_seen_at = now() WHERE id = $1`, b.id)
				return err
			}); err != nil && ctx.Err() == nil {
				slog.Warn("record heartbeat", "build_server", b.id, "err", err)
			}
		case msg.GetRenew() != nil:
			var cert []byte
			err := pgx.BeginFunc(ctx, h.Pool, func(tx pgx.Tx) error {
				var err error
				cert, err = h.CA.issue(ctx, tx, msg.GetRenew().GetCsr(), b.id, b.orgID)
				return err
			})
			if err != nil {
				slog.Warn("renew build server certificate", "build_server", b.id, "err", err)
				continue
			}
			if err := h.send(b, &v1.ConnectResponse{Body: &v1.ConnectResponse_Certificate{Certificate: cert}}); err != nil {
				return err
			}
		case msg.GetSandbox() != nil:
			st := msg.GetSandbox()
			if st.GetState() != v1.SandboxStatus_STATE_EXITED && st.GetState() != v1.SandboxStatus_STATE_FAILED {
				continue
			}
			h.mu.Lock()
			known := b.jobs[st.GetJobId()]
			delete(b.jobs, st.GetJobId())
			test := b.tests[st.GetJobId()]
			h.mu.Unlock()
			if test != nil {
				test <- st
			}
			if known && h.OnExit != nil {
				h.OnExit(st.GetJobId(), st)
			}
		}
	}
}
