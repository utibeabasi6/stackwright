package toolbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/utibeabasi6/stackwright/gen/stackwright/v1"
)

// newCheckout clones a bare repository whose main branch has main.tf, onto
// the branch agent/x. A pre-receive hook refuses pushes while refuse
// exists.
func newCheckout(t *testing.T) (b *box, bare, refuse string) {
	t.Helper()
	dir := t.TempDir()
	bare, seed, refuse := filepath.Join(dir, "remote.git"), filepath.Join(dir, "seed"), filepath.Join(dir, "refuse")
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "--bare", "--initial-branch=main", bare)
	writeFiles(t, seed, map[string]string{"main.tf": "# start\n"})
	run("-C", seed, "init", "--initial-branch=main")
	run("-C", seed, "add", "--all")
	run("-C", seed, "-c", "user.name=T", "-c", "user.email=t@a.test", "commit", "-m", "Start")
	run("-C", seed, "push", bare, "main")
	hook := "#!/bin/sh\nif [ -e " + refuse + " ]; then echo 'Write access to repository not granted.'; exit 1; fi\n"
	if err := os.WriteFile(filepath.Join(bare, "hooks", "pre-receive"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	b = &box{token: "test-token", work: dir, repo: filepath.Join(dir, "repo"), branch: "agent/x", base: "main"}
	ctx := context.Background()
	for _, args := range [][]string{{"clone", "--no-tags", bare, b.repo}, {"-C", b.repo, "checkout", "-B", "agent/x", "origin/main"}} {
		if _, err := b.git(ctx, dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	return b, bare, refuse
}

func TestCommitPushesTheKeptCommitAfterAFailedPush(t *testing.T) {
	b, bare, refuse := newCheckout(t)
	ctx := context.Background()
	writeFiles(t, b.repo, map[string]string{"main.tf": "# changed\n"})
	if err := os.WriteFile(refuse, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	order := &v1.CommitOrder{Message: "Change main.tf", AuthorName: "Owner", AuthorEmail: "owner@a.test"}
	if _, err := b.commit(ctx, order); err == nil || !strings.Contains(err.Error(), "Write access") {
		t.Fatalf("the first push: %v", err)
	}
	if !b.dirty(ctx) {
		t.Fatal("a commit whose push failed is not counted as a change")
	}
	if err := os.Remove(refuse); err != nil {
		t.Fatal(err)
	}
	sha, err := b.commit(ctx, &v1.CommitOrder{Message: "Second approval", AuthorName: "Owner", AuthorEmail: "owner@a.test"})
	if err != nil {
		t.Fatalf("the second approval: %v", err)
	}
	out, err := exec.Command("git", "--git-dir", bare, "log", "--format=%H %s", "agent/x").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), sha+" Change main.tf\n") {
		t.Errorf("the branch on the remote:\n%s", out)
	}
	if b.dirty(ctx) {
		t.Error("the checkout is dirty after the push")
	}
}

func TestDiscardDropsACommitWhosePushFailed(t *testing.T) {
	b, _, refuse := newCheckout(t)
	ctx := context.Background()
	writeFiles(t, b.repo, map[string]string{"main.tf": "# changed\n"})
	if err := os.WriteFile(refuse, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.commit(ctx, &v1.CommitOrder{Message: "Change", AuthorName: "Owner", AuthorEmail: "owner@a.test"}); err == nil {
		t.Fatal("the push did not fail")
	}
	if _, err := b.discard(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(b.repo, "main.tf"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "# start\n" || b.dirty(ctx) {
		t.Errorf("after discard: %q, dirty=%v", data, b.dirty(ctx))
	}
}
