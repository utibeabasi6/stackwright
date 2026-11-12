package toolbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"unicode/utf8"
)

const maxEdit = 1 << 20

// editorCall runs the editor's open_file and save_file, whose files travel
// as data, past the cut on a tool's output.
func (b *box) editorCall(ctx context.Context, name, raw string) (string, []byte, error) {
	if b.engine == "" {
		return "", nil, errors.New("the checkout is not ready")
	}
	var args struct {
		Path    string `json:"path"`
		Base    string `json:"base"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return "", nil, fmt.Errorf("the arguments are not JSON: %w", err)
	}
	if name == "open_file" {
		data, err := b.openFile(args.Path)
		return "", data, err
	}
	return b.saveFile(ctx, args.Path, args.Base, args.Content)
}

// openFile returns a file whole, for the editor. A file that is too large
// or not text is refused.
func (b *box) openFile(rel string) ([]byte, error) {
	root, name, err := b.open(rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(name)
	switch {
	case err != nil:
		return nil, err
	case len(data) > maxEdit:
		return nil, fmt.Errorf("%s is larger than %d KiB, which the editor opens", rel, maxEdit>>10)
	case !utf8.Valid(data):
		return nil, fmt.Errorf("%s is not text", rel)
	}
	return data, nil
}

// saveFile writes content to rel. When the file is no longer base, the
// three merge line by line; a conflict writes nothing (adr/0034). It
// returns "saved", "merged" or "conflict", and the file as it is now.
func (b *box) saveFile(ctx context.Context, rel, base, content string) (string, []byte, error) {
	if len(content) > maxEdit {
		return "", nil, fmt.Errorf("the file is larger than %d KiB, which the editor saves", maxEdit>>10)
	}
	root, name, err := b.open(rel)
	if err != nil {
		return "", nil, err
	}
	current, err := root.ReadFile(name)
	_ = root.Close()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", nil, err
	}
	if string(current) == base {
		if _, err := b.write(rel, content); err != nil {
			return "", nil, err
		}
		return "saved", []byte(content), nil
	}
	dir, err := os.MkdirTemp(b.work, "merge-")
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	paths := map[string][]byte{"current": current, "base": []byte(base), "saved": []byte(content)}
	for n, data := range paths {
		if err := os.WriteFile(filepath.Join(dir, n), data, 0o600); err != nil {
			return "", nil, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "merge-file", "-p", "current", "base", "saved")
	cmd.Dir = dir
	merged, err := cmd.Output()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit) && exit.ExitCode() > 0:
		return "conflict", current, nil
	case err != nil:
		return "", nil, fmt.Errorf("git merge-file: %w", err)
	}
	if _, err := b.write(rel, string(merged)); err != nil {
		return "", nil, err
	}
	return "merged", merged, nil
}
