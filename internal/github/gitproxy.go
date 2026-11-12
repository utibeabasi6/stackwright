package github

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/stackwright/stackwright/internal/authz"
)

const maxPush = 256 << 20

// ErrPushRefused is returned for a push that the Git proxy does not let
// through.
var ErrPushRefused = errors.New("push refused")

// GitProxy serves /git/<session>/ for agent sandboxes (adr/0030). It
// forwards Git's smart HTTP protocol to GitHub with the installation token,
// which never reaches the sandbox. A push may update only the session's
// branch, and it spends one approval that a person gave.
func (g *GitHub) GitProxy() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		session, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/git/"), "/")
		bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		sid, orgID, err := authz.SandboxToken(ctx, g.Pool, bearer)
		if err != nil || sid != session {
			http.Error(w, "a session's sandbox token is required", http.StatusUnauthorized)
			return
		}
		service := r.URL.Query().Get("service")
		push := rest == "git-receive-pack" || service == "git-receive-pack"
		switch {
		case r.Method == http.MethodGet && rest == "info/refs" && (service == "git-upload-pack" || service == "git-receive-pack"):
		case r.Method == http.MethodPost && (rest == "git-upload-pack" || rest == "git-receive-pack"):
		default:
			http.NotFound(w, r)
			return
		}

		err = pgx.BeginFunc(ctx, g.Pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('stackwright.org_id', $1, true)`, orgID); err != nil {
				return err
			}
			var repo, branch string
			var installation int64
			if err := tx.QueryRow(ctx, `SELECT rp.full_name, rp.installation_id, s.branch FROM agent_sessions s
				JOIN repos rp ON rp.id = s.repo_id WHERE s.id = $1`, sid).Scan(&repo, &installation, &branch); err != nil {
				return err
			}
			body, length := io.Reader(r.Body), r.ContentLength
			if push && r.Method == http.MethodPost {
				raw, err := io.ReadAll(io.LimitReader(r.Body, maxPush+1))
				if err != nil {
					return err
				}
				if len(raw) > maxPush {
					return fmt.Errorf("%w: the push is larger than %d bytes", ErrPushRefused, maxPush)
				}
				if err := checkPush(raw, r.Header.Get("Content-Encoding"), "refs/heads/"+branch); err != nil {
					return err
				}
				var proposal int64
				err = tx.QueryRow(ctx, `UPDATE agent_approvals SET used_at = now() WHERE (session_id, proposal_seq) = (
					SELECT session_id, proposal_seq FROM agent_approvals WHERE session_id = $1 AND used_at IS NULL
					ORDER BY proposal_seq LIMIT 1 FOR UPDATE) RETURNING proposal_seq`, sid).Scan(&proposal)
				if errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("%w: no approved proposal waits for this push", ErrPushRefused)
				}
				if err != nil {
					return err
				}
				body, length = bytes.NewReader(raw), int64(len(raw))
			}
			token, err := g.installationToken(ctx, installation)
			if err != nil {
				return err
			}
			// The approval is spent only if GitHub takes the push.
			return g.forward(ctx, w, r, body, length, fmt.Sprintf("%s/%s.git/%s", g.WebURL, repo, rest), token)
		})
		var refused interface{ Status() int }
		switch {
		case err == nil:
		case errors.Is(err, ErrPushRefused):
			http.Error(w, err.Error(), http.StatusForbidden)
		case errors.As(err, &refused):
		default:
			slog.Error("git proxy", "session", sid, "err", err)
			http.Error(w, "the Git proxy failed", http.StatusBadGateway)
		}
	})
}

// upstreamStatus is a GitHub answer that the proxy already wrote back; it
// still rolls the transaction back, so no approval is spent.
type upstreamStatus int

func (s upstreamStatus) Error() string { return fmt.Sprintf("GitHub answered %d", int(s)) }
func (s upstreamStatus) Status() int   { return int(s) }

func (g *GitHub) forward(ctx context.Context, w http.ResponseWriter, r *http.Request, body io.Reader, length int64, url, token string) error {
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, url, body) //nolint:gosec // the configured GitHub web URL, a stored repository and one of three Git paths
	if err != nil {
		return err
	}
	req.ContentLength = length
	for _, h := range []string{"Content-Type", "Content-Encoding", "Accept", "Accept-Encoding", "Git-Protocol"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	req.SetBasicAuth("x-access-token", token)
	resp, err := g.HTTP.Do(req) //nolint:gosec // the configured GitHub web URL, a stored repository and one of three Git paths
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	for _, h := range []string{"Content-Type", "Content-Encoding", "Cache-Control", "Expires", "Pragma"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return upstreamStatus(resp.StatusCode)
	}
	return nil
}

// checkPush reads the ref updates at the start of a receive-pack request
// and refuses any that is not an update of ref.
// https://git-scm.com/docs/gitprotocol-pack#_reference_update_request_and_packfile_transfer
func checkPush(raw []byte, encoding, ref string) error {
	var in io.Reader = bytes.NewReader(raw)
	if encoding == "gzip" {
		gz, err := gzip.NewReader(in)
		if err != nil {
			return err
		}
		in = gz
	}
	updates := 0
	for {
		var size [4]byte
		if _, err := io.ReadFull(in, size[:]); err != nil {
			return fmt.Errorf("%w: the request has no complete ref list", ErrPushRefused)
		}
		var n int
		if _, err := fmt.Sscanf(string(size[:]), "%04x", &n); err != nil {
			return fmt.Errorf("%w: a bad pkt-line length", ErrPushRefused)
		}
		if n == 0 {
			break
		}
		if n < 4 {
			return fmt.Errorf("%w: a bad pkt-line length", ErrPushRefused)
		}
		line := make([]byte, n-4)
		if _, err := io.ReadFull(in, line); err != nil {
			return fmt.Errorf("%w: the request has no complete ref list", ErrPushRefused)
		}
		cmd, _, _ := strings.Cut(strings.TrimSuffix(string(line), "\n"), "\x00")
		fields := strings.Fields(cmd)
		if len(fields) != 3 {
			continue
		}
		if fields[2] != ref {
			return fmt.Errorf("%w: a session may push only to %s, not %s", ErrPushRefused, ref, fields[2])
		}
		if strings.Trim(fields[1], "0") == "" {
			return fmt.Errorf("%w: a session may not delete its branch", ErrPushRefused)
		}
		updates++
	}
	if updates == 0 {
		return fmt.Errorf("%w: the push updates no ref", ErrPushRefused)
	}
	return nil
}
