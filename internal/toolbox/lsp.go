package toolbox

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/textproto"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/internal/egress"
)

const maxFrame = 8 << 20

// The browser names files under file:///repo; tofu-ls sees the checkout.
const browserRoot = "file:///repo"

type languageServer struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	stop  sync.Once
}

type languageServers struct {
	mu   sync.Mutex
	byID map[string]*languageServer
}

// lsp handles a frame from the server: it starts a tofu-ls for a new
// conn, writes the frame to it, or stops it on close.
func (b *box) lsp(ctx context.Context, f *v1.LspFrame, send func(*v1.ToolboxMessage) error) {
	b.servers.mu.Lock()
	ls := b.servers.byID[f.GetConn()]
	b.servers.mu.Unlock()
	if f.GetClose() {
		if ls != nil {
			b.stopServer(f.GetConn(), ls)
		}
		return
	}
	if ls == nil {
		var err error
		if ls, err = b.startServer(ctx, f.GetConn(), send); err != nil {
			slog.Warn("start tofu-ls", "err", err)
			_ = send(&v1.ToolboxMessage{Body: &v1.ToolboxMessage_Lsp{Lsp: &v1.LspFrame{Conn: f.GetConn(), Close: true}}})
			return
		}
	}
	data := bytes.ReplaceAll(f.GetData(), []byte(browserRoot), []byte("file://"+filepath.ToSlash(b.repo)))
	if _, err := fmt.Fprintf(ls.stdin, "Content-Length: %d\r\n\r\n%s", len(data), data); err != nil {
		slog.Warn("write to tofu-ls", "err", err)
	}
}

func (b *box) startServer(ctx context.Context, conn string, send func(*v1.ToolboxMessage) error) (*languageServer, error) {
	cmd := exec.CommandContext(ctx, "tofu-ls", "serve")
	cmd.Dir = b.repo
	cmd.Env = append([]string{"HOME=" + filepath.Join(b.work, "home"), "PATH=" + filepath.Dir(b.engine) + ":/usr/local/bin:/usr/bin:/bin",
		"TF_IN_AUTOMATION=1"}, egress.Env()...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	ls := &languageServer{cmd: cmd, stdin: stdin}
	b.servers.mu.Lock()
	if b.servers.byID == nil {
		b.servers.byID = map[string]*languageServer{}
	}
	b.servers.byID[conn] = ls
	b.servers.mu.Unlock()
	go func() {
		repo := []byte("file://" + filepath.ToSlash(b.repo))
		r := textproto.NewReader(bufio.NewReader(stdout))
		for {
			h, err := r.ReadMIMEHeader()
			if err != nil {
				break
			}
			n, err := strconv.Atoi(h.Get("Content-Length"))
			if err != nil || n < 0 || n > maxFrame {
				break
			}
			data := make([]byte, n)
			if _, err := io.ReadFull(r.R, data); err != nil {
				break
			}
			data = bytes.ReplaceAll(data, repo, []byte(browserRoot))
			if err := send(&v1.ToolboxMessage{Body: &v1.ToolboxMessage_Lsp{Lsp: &v1.LspFrame{Conn: conn, Data: data}}}); err != nil {
				break
			}
		}
		b.stopServer(conn, ls)
		_ = send(&v1.ToolboxMessage{Body: &v1.ToolboxMessage_Lsp{Lsp: &v1.LspFrame{Conn: conn, Close: true}}})
	}()
	return ls, nil
}

func (b *box) stopServer(conn string, ls *languageServer) {
	b.servers.mu.Lock()
	if b.servers.byID[conn] == ls {
		delete(b.servers.byID, conn)
	}
	b.servers.mu.Unlock()
	ls.stop.Do(func() {
		_ = ls.stdin.Close()
		if ls.cmd.Process != nil {
			_ = ls.cmd.Process.Kill()
		}
		_ = ls.cmd.Wait()
	})
}

// stopServers stops every tofu-ls, when the stream to the server ends.
func (b *box) stopServers() {
	b.servers.mu.Lock()
	all := b.servers.byID
	b.servers.byID = nil
	b.servers.mu.Unlock()
	for conn, ls := range all {
		b.stopServer(conn, ls)
	}
}
