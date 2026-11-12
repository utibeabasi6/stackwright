package toolbox

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/stackwright/stackwright/internal/toolbox/guard"
)

const (
	maxRead  = 200 << 10
	maxFiles = 1000
	maxHits  = 200
)

// inside returns rel's path under root, refusing paths that leave it.
func inside(root, rel string) (string, error) {
	clean := path.Clean("/" + filepath.ToSlash(rel))
	if why := guard.Refused(clean); why != "" {
		return "", fmt.Errorf("%w: %s: %s", ErrRefused, rel, why)
	}
	return filepath.Join(root, filepath.FromSlash(clean)), nil
}

func (b *box) call(ctx context.Context, name, raw string) (string, error) {
	if b.engine == "" {
		return "", errors.New("the checkout is not ready")
	}
	var args struct {
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
		Old     string `json:"old"`
		New     string `json:"new"`
		Content string `json:"content"`
	}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			return "", fmt.Errorf("the arguments are not JSON: %w", err)
		}
	}
	switch name {
	case "list_files":
		return b.list(args.Path)
	case "read_file":
		return b.read(args.Path)
	case "search":
		return b.search(args.Pattern, args.Path)
	case "edit_file":
		return b.edit(args.Path, args.Old, args.New)
	case "write_file":
		return b.write(args.Path, args.Content)
	case "fmt":
		return b.engineRun(ctx, "fmt", "-recursive", "-no-color")
	case "validate":
		if out, err := b.engineRun(ctx, "init", "-backend=false", "-input=false", "-no-color"); err != nil {
			return out, err
		}
		return b.engineRun(ctx, "validate", "-no-color")
	case "diff":
		return b.diff(ctx)
	}
	return "", fmt.Errorf("there is no tool %q", name)
}

// open opens rel through an os.Root, which also refuses a symlink that
// leads out of the checkout.
func (b *box) open(rel string) (*os.Root, string, error) {
	if _, err := inside(b.repo, rel); err != nil {
		return nil, "", err
	}
	root, err := os.OpenRoot(b.repo)
	if err != nil {
		return nil, "", err
	}
	return root, path.Clean("/" + filepath.ToSlash(rel))[1:], nil
}

func (b *box) list(rel string) (string, error) {
	start, err := inside(b.repo, rel)
	if err != nil {
		return "", err
	}
	var out []string
	err = filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		r, _ := filepath.Rel(b.repo, p)
		r = filepath.ToSlash(r)
		if r != "." && guard.Refused(r) != "" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			out = append(out, r)
		}
		if len(out) >= maxFiles {
			return fs.SkipAll
		}
		return nil
	})
	return strings.Join(out, "\n"), err
}

func (b *box) read(rel string) (string, error) {
	root, name, err := b.open(rel)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(name)
	if err != nil {
		return "", err
	}
	if len(data) > maxRead {
		return string(data[:maxRead]) + "\n[cut short]", nil
	}
	return string(data), nil
}

func (b *box) search(pattern, rel string) (string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("the pattern is not a Go regular expression: %w", err)
	}
	files, err := b.list(rel)
	if err != nil {
		return "", err
	}
	var hits []string
	for _, f := range strings.Split(files, "\n") {
		if f == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(b.repo, filepath.FromSlash(f)))
		if err != nil || bytes.IndexByte(data, 0) >= 0 {
			continue
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		for n := 1; sc.Scan(); n++ {
			if re.MatchString(sc.Text()) {
				hits = append(hits, fmt.Sprintf("%s:%d: %s", f, n, sc.Text()))
				if len(hits) >= maxHits {
					return strings.Join(hits, "\n") + "\n[more matches left out]", nil
				}
			}
		}
	}
	return strings.Join(hits, "\n"), nil
}

func (b *box) edit(rel, old, replacement string) (string, error) {
	current, err := b.read(rel)
	if err != nil {
		return "", err
	}
	switch strings.Count(current, old) {
	case 0:
		return "", errors.New("the old text is not in the file")
	case 1:
	default:
		return "", errors.New("the old text is in the file more than once; give more of it")
	}
	return b.write(rel, strings.Replace(current, old, replacement, 1))
}

// write replaces the file through a temporary file and a rename, so a stop
// in the middle leaves the old file or the new one, never half of one.
func (b *box) write(rel, content string) (string, error) {
	root, name, err := b.open(rel)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	if dir := path.Dir(name); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	tmp := name + ".stackwright-tmp"
	if err := root.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return "", err
	}
	if err := root.Rename(tmp, name); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(content), name), nil
}

func (b *box) diff(ctx context.Context) (string, error) {
	if _, err := b.git(ctx, b.repo, "add", "--intent-to-add", "--all"); err != nil {
		return "", err
	}
	return b.git(ctx, b.repo, "diff", "--no-color", "--stat", "--patch")
}

func (b *box) engineRun(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.engine, args...) //nolint:gosec // the engine binary, checked against its SHA-256
	cmd.Dir = b.dir
	cmd.Env = []string{"HOME=" + filepath.Join(b.work, "home"), "PATH=/usr/local/bin:/usr/bin:/bin", "TF_IN_AUTOMATION=1", "TF_INPUT=0"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s failed: %w\n%s", filepath.Base(b.engine), args[0], err, out)
	}
	return string(out), nil
}

// pack returns the checkout as a .tar.gz for a plan, without the paths
// that no tool may touch.
func (b *box) pack() ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(b.repo, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == b.repo {
			return err
		}
		rel, _ := filepath.Rel(b.repo, p)
		if guard.Refused(filepath.ToSlash(rel)) != "" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(p) //nolint:gosec // a walked path inside the checkout
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel), Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
