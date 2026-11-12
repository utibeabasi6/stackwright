package toolbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

const opened = "a = 1\nb = 2\nc = 3\nd = 4\ne = 5\n"

func TestSaveFileMergesWithAChangeSinceItOpened(t *testing.T) {
	b, _, _ := newCheckout(t)
	ctx := context.Background()
	file := filepath.Join(b.repo, "vars.tf")
	writeFiles(t, b.repo, map[string]string{"vars.tf": opened})

	if outcome, data, err := b.saveFile(ctx, "vars.tf", opened, "a = 10\nb = 2\nc = 3\nd = 4\ne = 5\n"); err != nil || outcome != "saved" {
		t.Fatalf("a save with no change since: %s %q %v", outcome, data, err)
	}
	// The agent changes the last line; the person's editor still holds the
	// file from before.
	writeFiles(t, b.repo, map[string]string{"vars.tf": "a = 10\nb = 2\nc = 3\nd = 4\ne = 50\n"})
	outcome, data, err := b.saveFile(ctx, "vars.tf", "a = 10\nb = 2\nc = 3\nd = 4\ne = 5\n", "a = 10\nb = 20\nc = 3\nd = 4\ne = 5\n")
	want := "a = 10\nb = 20\nc = 3\nd = 4\ne = 50\n"
	if err != nil || outcome != "merged" || string(data) != want {
		t.Fatalf("merge: %s %q %v", outcome, data, err)
	}
	if got, _ := os.ReadFile(file); string(got) != want {
		t.Errorf("the checkout after the merge: %q", got)
	}
}

func TestSaveFileWritesNothingOnAConflict(t *testing.T) {
	b, _, _ := newCheckout(t)
	ctx := context.Background()
	agent := "a = 1\nb = 99\nc = 3\nd = 4\ne = 5\n"
	writeFiles(t, b.repo, map[string]string{"vars.tf": agent})
	outcome, data, err := b.saveFile(ctx, "vars.tf", opened, "a = 1\nb = 42\nc = 3\nd = 4\ne = 5\n")
	if err != nil || outcome != "conflict" || string(data) != agent {
		t.Fatalf("conflict: %s %q %v", outcome, data, err)
	}
	if got, _ := os.ReadFile(filepath.Join(b.repo, "vars.tf")); string(got) != agent {
		t.Errorf("a conflict wrote the file: %q", got)
	}
}

func TestEditorRefusesStateFilesAndBinaries(t *testing.T) {
	b, _, _ := newCheckout(t)
	writeFiles(t, b.repo, map[string]string{"terraform.tfstate": "{}", "logo.png": "\xff\xd8\xff"})
	for _, p := range []string{"terraform.tfstate", "logo.png", "../outside.tf"} {
		if _, err := b.openFile(p); err == nil {
			t.Errorf("opened %s", p)
		}
	}
	if _, _, err := b.saveFile(context.Background(), ".env", "", "SECRET=1"); err == nil {
		t.Error("saved .env")
	}
}
