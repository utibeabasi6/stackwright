package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// cli runs OpenTofu or Terraform in one folder. Both take the same flags and
// write the same -json lines, except that Terraform's init has no -json.
type cli struct {
	bin      string
	dir      string
	env      []string
	initJSON bool
	// line gets every output line, already redacted.
	line func(event)
	// redact hides secrets before a line goes anywhere.
	redact func([]byte) []byte
}

type event struct {
	typ          string
	resourceAddr string
	payload      []byte
}

func (c *cli) init(ctx context.Context) error {
	args := []string{"init", "-input=false"}
	if c.initJSON {
		args = append(args, "-json")
	}
	_, err := c.stream(ctx, args...)
	return err
}

// plan reports whether the plan has changes.
func (c *cli) plan(ctx context.Context, out string) (bool, error) {
	code, err := c.stream(ctx, "plan", "-json", "-input=false", "-detailed-exitcode", "-out="+out)
	return code == 2, err
}

func (c *cli) show(ctx context.Context, plan string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := c.command(ctx, "show", "-json", plan)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("show: %w: %s", err, c.redact(stderr.Bytes()))
	}
	return c.redact(stdout.Bytes()), nil
}

func (c *cli) apply(ctx context.Context, plan string) error {
	_, err := c.stream(ctx, "apply", "-json", "-input=false", plan)
	return err
}

// stream turns each output line into one event and returns the exit code.
// Plan's exit code 2 means changes, not failure.
func (c *cli) stream(ctx context.Context, args ...string) (int, error) {
	cmd := c.command(ctx, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return -1, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		e := parse(c.redact(scanner.Bytes()))
		c.line(e)
	}
	if err := scanner.Err(); err != nil {
		_, _ = io.Copy(io.Discard, stdout)
		_ = cmd.Wait()
		return -1, fmt.Errorf("read %s output: %w", args[0], err)
	}
	err = cmd.Wait()
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr) && exitErr.ExitCode() == 2 && args[0] == "plan":
		return 2, nil
	case errors.As(err, &exitErr):
		return exitErr.ExitCode(), fmt.Errorf("%s exited with %d", args[0], exitErr.ExitCode())
	case err != nil:
		return -1, err
	}
	return 0, nil
}

// command interrupts the engine on cancel, so it can release its state
// lock, and kills it 30 s later.
func (c *cli) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.bin, args...) //nolint:gosec // the engine binary, checked against its SHA-256
	cmd.Dir = c.dir
	cmd.Env = c.env
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 30 * time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// parse turns one output line into an event of type "engine." plus the
// line's type. A line that is not JSON becomes engine.log. The line types:
// https://opentofu.org/docs/internals/machine-readable-ui/
func parse(line []byte) event {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(line, &doc); err != nil {
		payload, _ := json.Marshal(map[string]string{"@message": string(line)})
		return event{typ: "engine.log", payload: payload}
	}
	var lineType string
	_ = json.Unmarshal(doc["type"], &lineType)
	e := event{typ: "engine.log", payload: bytes.Clone(line)}
	if lineType != "" {
		e.typ = "engine." + lineType
	}

	var v struct {
		Change     struct{ Resource struct{ Addr string } }
		Hook       struct{ Resource struct{ Addr string } }
		Diagnostic struct{ Address string }
	}
	_ = json.Unmarshal(line, &v)
	e.resourceAddr = v.Diagnostic.Address
	for _, addr := range []string{v.Hook.Resource.Addr, v.Change.Resource.Addr} {
		if addr != "" {
			e.resourceAddr = addr
		}
	}

	if lineType == "outputs" {
		var outputs map[string]map[string]json.RawMessage
		if json.Unmarshal(doc["outputs"], &outputs) == nil {
			for _, o := range outputs {
				var sensitive bool
				_ = json.Unmarshal(o["sensitive"], &sensitive)
				if sensitive {
					delete(o, "value")
				}
			}
			doc["outputs"], _ = json.Marshal(outputs)
			e.payload, _ = json.Marshal(doc)
		}
	}
	return e
}
