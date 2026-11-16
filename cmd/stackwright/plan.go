package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
)

// maxUpload matches the server's limit on an upload.
const maxUpload = 32 << 20

// runPlan uploads the working tree and plans it. The run is speculative:
// nothing can apply it (adr/0044).
func runPlan(args []string) error {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	tf := addTargetFlags(fs)
	_ = fs.Parse(args)
	a, err := signedIn()
	if err != nil {
		return err
	}
	ctx := context.Background()
	t, err := a.find(ctx, tf)
	if err != nil {
		return err
	}
	if t.root == "" {
		return errors.New("plan runs in a git repository, so it can upload what git tracks")
	}
	// Tracked files, and untracked ones that .gitignore does not exclude.
	list, err := exec.CommandContext(ctx, "git", "-C", t.root, "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output() //nolint:gosec // git in the user's repository
	if err != nil {
		return fmt.Errorf("list the files: %w", err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, name := range strings.Split(strings.TrimRight(string(list), "\x00"), "\x00") {
		if name == "" || strings.Contains("/"+name+"/", "/.terraform/") {
			continue
		}
		path := filepath.Join(t.root, filepath.FromSlash(name))
		fi, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		// A link is uploaded as a link: the configuration it stands for
		// must not go missing from the plan.
		if fi.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: name, Linkname: target, ModTime: fi.ModTime()}); err != nil {
				return err
			}
			continue
		}
		if !fi.Mode().IsRegular() {
			continue
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: int64(fi.Mode().Perm()), Size: fi.Size(), ModTime: fi.ModTime()}); err != nil {
			return err
		}
		f, err := os.Open(path) //nolint:gosec // a file git lists in the user's repository
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		_ = f.Close()
		if err != nil {
			return err
		}
		if buf.Len() > maxUpload {
			return fmt.Errorf("the working tree is larger than %d MiB packed", maxUpload>>20)
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	host, _ := os.Hostname()
	created, err := a.runs.CreateRun(ctx, connect.NewRequest(&v1.CreateRunRequest{Org: t.org, Project: t.project, Workspace: t.workspace,
		Speculative: true, Source: buf.Bytes(), SourceName: "cli on " + host + ":" + filepath.Base(t.root)}))
	if err != nil {
		return err
	}
	n := created.Msg.GetRun().GetNumber()
	fmt.Printf("Plan #%d of %s/%s/%s: %s/%s/projects/%s/%s/runs/%d\n\n", n, t.org, t.project, t.workspace, a.server, t.org, t.project, t.workspace, n)
	run, err := a.follow(ctx, os.Stdout, t, n, true)
	if err != nil {
		return err
	}
	if run.GetStatus() != v1.RunStatus_RUN_STATUS_PLANNED {
		return fmt.Errorf("plan #%d %s", n, status(run.GetStatus()))
	}
	return nil
}
