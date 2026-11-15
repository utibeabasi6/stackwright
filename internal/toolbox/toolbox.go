// Package toolbox runs in an agent session's sandbox. It keeps the
// session's checkout and runs the tools, commits and discards that the
// server orders, over a stream it opens to the server (adr/0030).
package toolbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/gen/stackwright/v1/stackwrightv1connect"
	"github.com/stackwright/stackwright/internal/egress"
	"github.com/stackwright/stackwright/internal/executor"
)

const (
	commandTimeout = 5 * time.Minute
	maxOutput      = 64 << 10
)

// ErrRefused is returned for a path that no tool may touch.
var ErrRefused = errors.New("refused")

type box struct {
	token  string
	work   string
	cache  string
	repo   string
	dir    string
	branch string
	base   string
	engine string
	// servers are the tofu-ls processes, one for each editor WebSocket.
	servers languageServers
}

// Run serves the server's orders until ctx ends or the server ends the
// session. A dropped stream is opened again.
func Run(ctx context.Context, serverURL, token, work, cache string) error {
	protocols := new(http.Protocols)
	protocols.SetHTTP2(true)
	protocols.SetUnencryptedHTTP2(true)
	// HTTP/2 without TLS cannot pass a forward proxy, so the stream goes
	// through a CONNECT tunnel (adr/0046).
	client := stackwrightv1connect.NewToolboxServiceClient(&http.Client{Transport: &http.Transport{Protocols: protocols,
		DialContext: egress.Dial}}, serverURL)
	b := &box{token: token, work: work, cache: cache, repo: filepath.Join(work, "repo")}
	for {
		err := b.serve(ctx, client)
		switch {
		case ctx.Err() != nil:
			return nil
		case connect.CodeOf(err) == connect.CodeUnauthenticated || connect.CodeOf(err) == connect.CodeNotFound:
			return err
		}
		slog.Warn("toolbox stream ended; connecting again", "err", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *box) serve(ctx context.Context, client stackwrightv1connect.ToolboxServiceClient) error {
	stream := client.Connect(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+b.token)
	defer func() { _ = stream.CloseRequest() }()
	defer b.stopServers()
	// A send fails with io.EOF when the server ended the stream; the reason
	// comes from Receive. Language servers send from their own goroutines.
	var sending sync.Mutex
	send := func(m *v1.ToolboxMessage) error {
		sending.Lock()
		defer sending.Unlock()
		err := stream.Send(m)
		if errors.Is(err, io.EOF) {
			_, err = stream.Receive()
		}
		return err
	}
	if err := send(&v1.ToolboxMessage{Body: &v1.ToolboxMessage_Hello{Hello: &v1.ToolboxHello{}}}); err != nil {
		return err
	}
	if b.engine != "" {
		if err := send(&v1.ToolboxMessage{Body: &v1.ToolboxMessage_Ready{Ready: &v1.ToolboxReady{Dirty: b.dirty(ctx)}}}); err != nil {
			return err
		}
	}
	for {
		order, err := stream.Receive()
		if err != nil {
			return err
		}
		var msg *v1.ToolboxMessage
		switch o := order.GetBody().(type) {
		case *v1.ToolboxOrder_Checkout:
			if err := b.checkout(ctx, o.Checkout); err != nil {
				return fmt.Errorf("checkout: %w", err)
			}
			slog.Info("checkout ready", "branch", b.branch)
			msg = &v1.ToolboxMessage{Body: &v1.ToolboxMessage_Ready{Ready: &v1.ToolboxReady{Dirty: b.dirty(ctx)}}}
		case *v1.ToolboxOrder_Lsp:
			b.lsp(ctx, o.Lsp, send)
			continue
		case *v1.ToolboxOrder_Call:
			id := o.Call.GetId()
			switch o.Call.GetName() {
			case "pack":
				data, err := b.pack()
				msg = b.result(ctx, id, fmt.Sprintf("%d bytes", len(data)), err)
				msg.GetResult().Data = data
			case "open_file", "save_file":
				out, data, err := b.editorCall(ctx, o.Call.GetName(), o.Call.GetArguments())
				msg = b.result(ctx, id, out, err)
				msg.GetResult().Data = data
			default:
				out, err := b.call(ctx, o.Call.GetName(), o.Call.GetArguments())
				msg = b.result(ctx, id, out, err)
			}
		case *v1.ToolboxOrder_Commit:
			out, err := b.commit(ctx, o.Commit)
			msg = b.result(ctx, o.Commit.GetId(), out, err)
		case *v1.ToolboxOrder_Discard:
			out, err := b.discard(ctx)
			msg = b.result(ctx, o.Discard.GetId(), out, err)
		default:
			continue
		}
		if err := send(msg); err != nil {
			return err
		}
	}
}

func (b *box) result(ctx context.Context, id, out string, err error) *v1.ToolboxMessage {
	r := &v1.ToolResult{Id: id, Output: out, Dirty: b.dirty(ctx)}
	if err != nil {
		r.Error = true
		r.Output = err.Error()
	}
	if len(r.Output) > maxOutput {
		r.Output = r.Output[:maxOutput] + "\n[cut short]"
	}
	return &v1.ToolboxMessage{Body: &v1.ToolboxMessage_Result{Result: r}}
}

// checkout clones the repository through the server's Git proxy and checks
// out the session's branch, or a new one from base. A checkout that exists
// is kept, so a restarted stream does not lose the changes.
func (b *box) checkout(ctx context.Context, c *v1.Checkout) error {
	b.branch, b.base = c.GetBranch(), c.GetBase()
	if _, err := os.Stat(filepath.Join(b.repo, ".git")); err != nil {
		if _, err := b.git(ctx, b.work, "clone", "--no-tags", c.GetGitUrl(), b.repo); err != nil {
			return err
		}
		start := "origin/" + c.GetBase()
		if out, err := b.git(ctx, b.repo, "ls-remote", "--heads", "origin", c.GetBranch()); err == nil && strings.TrimSpace(out) != "" {
			if _, err := b.git(ctx, b.repo, "fetch", "origin", c.GetBranch()); err != nil {
				return err
			}
			start = "origin/" + c.GetBranch()
		}
		if _, err := b.git(ctx, b.repo, "checkout", "-B", c.GetBranch(), start); err != nil {
			return err
		}
		// validate runs init, which writes .terraform; no commit may take it.
		exclude := filepath.Join(b.repo, ".git", "info", "exclude")
		if err := os.WriteFile(exclude, []byte(".terraform/\n*.tfstate\n*.tfstate.backup\n"), 0o600); err != nil {
			return err
		}
	}
	dir, err := inside(b.repo, c.GetPath())
	if err != nil {
		return err
	}
	b.dir = dir
	engine, _, err := executor.FetchEngine(ctx, c.GetEngine(), b.cache, b.work)
	if err != nil {
		return err
	}
	b.engine = engine
	return nil
}

// dirty reports changes that are not committed, or commits that are not
// pushed, such as one whose push failed.
func (b *box) dirty(ctx context.Context) bool {
	if out, err := b.git(ctx, b.repo, "status", "--porcelain"); err == nil && out != "" {
		return true
	}
	ahead, err := b.git(ctx, b.repo, "rev-list", "--count", "HEAD", "--not", "--remotes=origin")
	return err == nil && ahead != "0"
}

func (b *box) commit(ctx context.Context, c *v1.CommitOrder) (string, error) {
	if !b.dirty(ctx) {
		return "", errors.New("there is nothing to commit")
	}
	// A commit whose push failed is pushed as it is.
	if out, err := b.git(ctx, b.repo, "status", "--porcelain"); err != nil {
		return "", err
	} else if out != "" {
		if _, err := b.git(ctx, b.repo, "add", "--all"); err != nil {
			return "", err
		}
		author := fmt.Sprintf("%s <%s>", c.GetAuthorName(), c.GetAuthorEmail())
		if _, err := b.git(ctx, b.repo, "-c", "user.name="+c.GetAuthorName(), "-c", "user.email="+c.GetAuthorEmail(),
			"commit", "--author", author, "-m", c.GetMessage()); err != nil {
			return "", err
		}
	}
	if _, err := b.git(ctx, b.repo, "push", "origin", "HEAD:refs/heads/"+b.branch); err != nil {
		// The commit stays local, so a second approval can push it.
		return "", err
	}
	return b.git(ctx, b.repo, "rev-parse", "HEAD")
}

// discard goes back to the branch as it was last pushed, or to its base
// when it was never pushed, so a commit whose push failed goes too.
func (b *box) discard(ctx context.Context) (string, error) {
	pushed := "origin/" + b.branch
	if _, err := b.git(ctx, b.repo, "rev-parse", "--verify", "--quiet", pushed); err != nil {
		pushed = "origin/" + b.base
	}
	if _, err := b.git(ctx, b.repo, "reset", "--hard", pushed); err != nil {
		return "", err
	}
	return b.git(ctx, b.repo, "clean", "-fd")
}

// git runs git with the session token as the Git proxy's credential. The
// token travels in GIT_CONFIG_*, never in a remote URL or an argument.
func (b *box) git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // subcommands and arguments this package builds
	cmd.Dir = dir
	cmd.Env = []string{
		"HOME=" + filepath.Join(b.work, "home"),
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Bearer " + b.token,
		"GIT_CONFIG_KEY_1=safe.directory",
		"GIT_CONFIG_VALUE_1=*",
	}
	cmd.Env = append(cmd.Env, egress.Env()...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.ReplaceAll(string(out), b.token, "[token]"))
	}
	return strings.TrimSpace(string(out)), nil
}
