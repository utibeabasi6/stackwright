package api

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/utibeabasi6/stackwright/gen/stackwright/v1"
	"github.com/utibeabasi6/stackwright/internal/auth"
	"github.com/utibeabasi6/stackwright/internal/authz"
	"github.com/utibeabasi6/stackwright/internal/builders"
	"github.com/utibeabasi6/stackwright/internal/seal"
)

const enrolTokenTTL = time.Hour

type BuildServers struct {
	KEK      seal.KEK
	Hub      *builders.Hub
	Binaries map[string]builders.Binary
}

func formatLabels(labels map[string]string) string {
	parts := make([]string, 0, len(labels))
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		parts = append(parts, k+"="+labels[k])
	}
	return strings.Join(parts, ",")
}

const serverColumns = `id, name, org_id IS NULL, labels, slots, draining, platform, version, enrolled_via, ssh_host, last_seen_at, created_at`

func (h BuildServers) scanServer(row pgx.Row) (*v1.BuildServer, error) {
	b := &v1.BuildServer{}
	var lastSeen *time.Time
	var created time.Time
	var draining bool
	if err := row.Scan(&b.Id, &b.Name, &b.BuiltIn, &b.Labels, &b.Slots, &draining, &b.Platform, &b.Version, &b.EnrolledVia,
		&b.SshHost, &lastSeen, &created); err != nil {
		return nil, err
	}
	b.CreatedAt = timestamppb.New(created)
	if lastSeen != nil {
		b.LastSeenAt = timestamppb.New(*lastSeen)
	}
	b.Status = v1.BuildServer_STATUS_OFFLINE
	if live, ok := h.Hub.Status()[b.Id]; ok {
		b.Status, b.Running, b.Upgrade = v1.BuildServer_STATUS_ONLINE, int32(min(live.Running, 64)), live.Upgrade //nolint:gosec // capped
		if draining {
			b.Status = v1.BuildServer_STATUS_DRAINING
		}
	}
	return b, nil
}

func (h BuildServers) ListBuildServers(ctx context.Context, req *connect.Request[v1.ListBuildServersRequest]) (*connect.Response[v1.ListBuildServersResponse], error) {
	s := authz.ScopeFrom(ctx)
	rows, err := s.Tx.Query(ctx, `SELECT `+serverColumns+` FROM build_servers WHERE removed_at IS NULL ORDER BY org_id NULLS FIRST, name`)
	if err != nil {
		return nil, authz.Internal(err)
	}
	servers, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*v1.BuildServer, error) { return h.scanServer(r) })
	if err != nil {
		return nil, authz.Internal(err)
	}
	return connect.NewResponse(&v1.ListBuildServersResponse{BuildServers: servers}), nil
}

// orgServer refuses the built-in build server, which .env configures.
func orgServer(ctx context.Context, tx pgx.Tx, id string) (string, error) {
	if uuid.Validate(id) != nil {
		return "", connect.NewError(connect.CodeNotFound, errors.New("build server not found"))
	}
	var name string
	var builtIn bool
	err := tx.QueryRow(ctx, `SELECT name, org_id IS NULL FROM build_servers WHERE id = $1 AND removed_at IS NULL`, id).Scan(&name, &builtIn)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", connect.NewError(connect.CodeNotFound, errors.New("build server not found"))
	}
	if err != nil {
		return "", authz.Internal(err)
	}
	if builtIn {
		return "", connect.NewError(connect.CodeFailedPrecondition, errors.New("the built-in build server is set in the instance's .env"))
	}
	return name, nil
}

func (h BuildServers) UpdateBuildServer(ctx context.Context, req *connect.Request[v1.UpdateBuildServerRequest]) (*connect.Response[v1.UpdateBuildServerResponse], error) {
	s := authz.ScopeFrom(ctx)
	m := req.Msg
	name, err := orgServer(ctx, s.Tx, m.GetId())
	if err != nil {
		return nil, err
	}
	var changes []string
	var labels map[string]string
	var slots *int
	if m.Labels != nil {
		labels = m.GetLabels().GetValues()
		if labels == nil {
			labels = map[string]string{}
		}
		if err := builders.CheckLabels(labels); err != nil {
			return nil, err
		}
		changes = append(changes, "labels="+formatLabels(labels))
	}
	if m.Slots != nil {
		if m.GetSlots() < 1 || m.GetSlots() > 64 {
			return nil, invalid("a build server has 1 to 64 slots")
		}
		n := int(m.GetSlots())
		slots = &n
		changes = append(changes, fmt.Sprintf("slots=%d", n))
	}
	if m.Draining != nil {
		changes = append(changes, fmt.Sprintf("draining=%t", m.GetDraining()))
	}
	if len(changes) == 0 {
		return nil, invalid("nothing to change")
	}
	raw, _ := json.Marshal(labels)
	server, err := h.scanServer(s.Tx.QueryRow(ctx, `UPDATE build_servers SET
		labels = coalesce($2::jsonb, labels), slots = coalesce($3, slots), draining = coalesce($4, draining)
		WHERE id = $1 RETURNING `+serverColumns, m.GetId(), nullJSON(labels, raw), m.Slots, m.Draining))
	if err != nil {
		return nil, authz.Internal(err)
	}
	s.Target(fmt.Sprintf("build server %s %s", name, strings.Join(changes, " ")))
	s.AfterCommit(func() { h.Hub.Update(m.GetId(), labels, slots, m.Draining) })
	return connect.NewResponse(&v1.UpdateBuildServerResponse{BuildServer: server}), nil
}

func nullJSON(m map[string]string, raw []byte) []byte {
	if m == nil {
		return nil
	}
	return raw
}

func (h BuildServers) RemoveBuildServer(ctx context.Context, req *connect.Request[v1.RemoveBuildServerRequest]) (*connect.Response[v1.RemoveBuildServerResponse], error) {
	s := authz.ScopeFrom(ctx)
	name, err := orgServer(ctx, s.Tx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	if _, err := s.Tx.Exec(ctx, `UPDATE build_servers SET removed_at = now(), draining = true WHERE id = $1`, req.Msg.GetId()); err != nil {
		return nil, authz.Internal(err)
	}
	if _, err := s.Tx.Exec(ctx, `UPDATE build_server_certs SET revoked_at = now() WHERE build_server_id = $1 AND revoked_at IS NULL`,
		req.Msg.GetId()); err != nil {
		return nil, authz.Internal(err)
	}
	s.Target("build server " + name)
	s.AfterCommit(func() { h.Hub.Disconnect(req.Msg.GetId()) })
	return connect.NewResponse(&v1.RemoveBuildServerResponse{}), nil
}

func (h BuildServers) GetSSHKey(ctx context.Context, req *connect.Request[v1.GetSSHKeyRequest]) (*connect.Response[v1.GetSSHKeyResponse], error) {
	s := authz.ScopeFrom(ctx)
	_, public, err := builders.SSHKey(ctx, s.Tx, h.KEK, s.OrgID, "stackwright-"+req.Msg.GetOrg())
	if err != nil {
		return nil, authz.Internal(err)
	}
	s.Target("ssh key")
	return connect.NewResponse(&v1.GetSSHKeyResponse{PublicKey: public}), nil
}

func checkHost(host string, port int32) (int, error) {
	if host == "" || len(host) > 253 || strings.ContainsAny(host, " /@\t\n") {
		return 0, invalid("the host is a name or an IP address")
	}
	if port == 0 {
		port = 22
	}
	if port < 1 || port > 65535 {
		return 0, invalid("the port is 1 to 65535")
	}
	return int(port), nil
}

func (h BuildServers) ScanHostKey(ctx context.Context, req *connect.Request[v1.ScanHostKeyRequest]) (*connect.Response[v1.ScanHostKeyResponse], error) {
	s := authz.ScopeFrom(ctx)
	port, err := checkHost(req.Msg.GetHost(), req.Msg.GetPort())
	if err != nil {
		return nil, err
	}
	key, fingerprint, err := builders.ScanHostKey(ctx, req.Msg.GetHost(), port)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	s.Target(fmt.Sprintf("host key %s:%d", req.Msg.GetHost(), port))
	return connect.NewResponse(&v1.ScanHostKeyResponse{HostKey: key, Fingerprint: fingerprint}), nil
}

// newEnrolToken records a one-time token for a build server that is not
// enrolled yet. tx needs the org setting.
func newEnrolToken(ctx context.Context, tx pgx.Tx, s *authz.Scope, name string, labels map[string]string, slots int32,
	via, host string, port int, user, hostKey string) (serverID, token string, expires time.Time, err error) {
	if err := builders.CheckServer(name, slots, labels); err != nil {
		return "", "", time.Time{}, err
	}
	var taken bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM build_servers WHERE org_id = app_org_id() AND name = $1 AND removed_at IS NULL)`,
		name).Scan(&taken); err != nil {
		return "", "", time.Time{}, authz.Internal(err)
	}
	if taken {
		return "", "", time.Time{}, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("the org already has a build server named %s", name))
	}
	if labels == nil {
		labels = map[string]string{}
	}
	serverID, token, expires = uuid.NewString(), "swe_"+auth.RandomToken(), time.Now().Add(enrolTokenTTL)
	_, err = tx.Exec(ctx, `INSERT INTO enrol_tokens (token_hash, build_server_id, org_id, name, labels, slots, via,
		ssh_host, ssh_port, ssh_user, ssh_host_key, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		auth.HashToken(token), serverID, s.OrgID, name, labels, slots, via, host, port, user, hostKey, s.UserID, expires)
	if err != nil {
		return "", "", time.Time{}, authz.Internal(err)
	}
	return serverID, token, expires, nil
}

func (h BuildServers) CreateJoinCommand(ctx context.Context, req *connect.Request[v1.CreateJoinCommandRequest]) (*connect.Response[v1.CreateJoinCommandResponse], error) {
	s := authz.ScopeFrom(ctx)
	m := req.Msg
	amd, arm := h.Binaries["linux_amd64"], h.Binaries["linux_arm64"]
	if amd.SHA256 == "" && arm.SHA256 == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this server has no builder binaries to hand out"))
	}
	_, token, expires, err := newEnrolToken(ctx, s.Tx, s, m.GetName(), m.GetLabels(), m.GetSlots(), "command", "", 0, "", "")
	if err != nil {
		return nil, err
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: h.Hub.CA.Certificate()})
	command := fmt.Sprintf(`set -eu
case "$(uname -m)" in
  x86_64) platform=linux_amd64; sum=%s ;;
  aarch64|arm64) platform=linux_arm64; sum=%s ;;
  *) echo "stackwright: $(uname -m) is not supported" >&2; exit 1 ;;
esac
cat > /tmp/stackwright-ca.pem <<'EOF'
%sEOF
curl -fsSL --cacert /tmp/stackwright-ca.pem -o /tmp/stackwright-builder %s/downloads/stackwright-builder-$platform
echo "$sum  /tmp/stackwright-builder" | sha256sum -c -
install -m 0755 /tmp/stackwright-builder /usr/local/bin/stackwright-builder
echo '%s' | stackwright-builder enrol --server %s --ca-sha256 %s
stackwright-builder install
`, amd.SHA256, arm.SHA256, ca, h.Hub.BuilderURL, token, h.Hub.BuilderURL, h.Hub.CA.Fingerprint())
	s.Target("join command for build server " + m.GetName())
	return connect.NewResponse(&v1.CreateJoinCommandResponse{Command: command, ExpiresAt: timestamppb.New(expires)}), nil
}

func (h BuildServers) EnrolOverSSH(ctx context.Context, req *connect.Request[v1.EnrolOverSSHRequest], stream *connect.ServerStream[v1.EnrolOverSSHResponse]) error {
	s := authz.ScopeFrom(ctx)
	m := req.Msg
	port, err := checkHost(m.GetHost(), m.GetPort())
	if err != nil {
		return err
	}
	if m.GetUser() == "" || strings.ContainsAny(m.GetUser(), " '\"\\") {
		return invalid("the SSH user is a plain user name")
	}
	if m.GetHostKey() == "" {
		return invalid("confirm the host key first")
	}
	t := builders.Target{Host: m.GetHost(), Port: port, User: m.GetUser(), HostKey: m.GetHostKey()}
	var serverID, token string
	err = s.Read(ctx, h.Hub.Pool, func(tx pgx.Tx) error {
		var err error
		if t.Signer, _, err = builders.SSHKey(ctx, tx, h.KEK, s.OrgID, "stackwright-"+m.GetOrg()); err != nil {
			return authz.Internal(err)
		}
		if serverID, token, _, err = newEnrolToken(ctx, tx, s, m.GetName(), m.GetLabels(), m.GetSlots(), "ssh",
			m.GetHost(), port, m.GetUser(), m.GetHostKey()); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO audit_log (org_id, actor_kind, actor_id, action, target, ip)
			VALUES ($1, 'user', $2, 'BuildServerService/EnrolOverSSH', $3, nullif($4, '')::inet)`,
			s.OrgID, s.UserID, fmt.Sprintf("build server %s at %s@%s:%d", m.GetName(), m.GetUser(), m.GetHost(), port), s.IP)
		return err
	})
	if err != nil {
		return err
	}
	step := func(text string) { _ = stream.Send(&v1.EnrolOverSSHResponse{Step: text}) }
	err = builders.InstallOverSSH(ctx, t, builders.Install{
		Binaries: h.Binaries, BuilderURL: h.Hub.BuilderURL, CA: h.Hub.CA.Fingerprint(), Token: token, Step: step,
	})
	switch {
	case errors.Is(err, builders.ErrNoDocker):
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"no Docker on %s: install Docker Engine (https://docs.docker.com/engine/install/), then enrol again. Nothing was installed", m.GetHost()))
	case err != nil:
		return connect.NewError(connect.CodeFailedPrecondition, err)
	}

	step("Waiting for the builder to connect")
	wait, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if err := h.Hub.WaitOnline(wait, serverID); err != nil {
		return connect.NewError(connect.CodeDeadlineExceeded, errors.New(
			"the builder is installed but did not connect in 90 s; run `journalctl -u stackwright-builder` on the host"))
	}
	step("Running a test sandbox")
	test, cancelTest := context.WithTimeout(ctx, 5*time.Minute)
	defer cancelTest()
	if err := h.Hub.Test(test, serverID); err != nil {
		return connect.NewError(connect.CodeFailedPrecondition, err)
	}
	var server *v1.BuildServer
	if err := s.Read(ctx, h.Hub.Pool, func(tx pgx.Tx) error {
		server, err = h.scanServer(tx.QueryRow(ctx, `SELECT `+serverColumns+` FROM build_servers WHERE id = $1`, serverID))
		return err
	}); err != nil {
		return authz.Internal(err)
	}
	return stream.Send(&v1.EnrolOverSSHResponse{Step: "Done", Done: true, BuildServer: server})
}
