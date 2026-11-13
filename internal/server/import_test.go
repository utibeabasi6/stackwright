package server_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
)

// petState is a state whose one random_pet matches randomPet, so a plan of
// the two has no changes.
func petState(lineage string, serial int) string {
	return fmt.Sprintf(`{"version": 4, "terraform_version": "1.10.7", "serial": %d, "lineage": %q, "outputs": {}, "resources": [
  {"mode": "managed", "type": "random_pet", "name": "web", "provider": "provider[\"registry.opentofu.org/hashicorp/random\"]",
   "instances": [{"schema_version": 0, "attributes": {"id": "famous-cat", "keepers": null, "length": 2, "prefix": null, "separator": "-"}}]}]}`,
		serial, lineage)
}

const randomPet = `terraform {
  required_providers {
    random = { source = "hashicorp/random" }
  }
}

resource "random_pet" "web" {
  length = 2
}
`

func TestImportFindsRootsAndPlansEachWithNoChanges(t *testing.T) {
	requireDocker(t)
	w := start(t, nil, true)
	owner := w.setup()
	w.createApp(owner)
	w.link(owner)
	w.startBuilder(4)

	// remote/ keeps its state in an S3 bucket that its code names; the
	// sandbox reaches the test object store as the executor does.
	key := "infra/remote.tfstate"
	state := petState("remote-lineage", 7)
	if _, err := w.s3.PutObject(context.Background(), w.bucket, key, strings.NewReader(state), int64(len(state)),
		minio.PutObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	backend := fmt.Sprintf(`terraform {
  backend "s3" {
    bucket                      = %q
    key                         = %q
    region                      = "us-east-1"
    access_key                  = %q
    secret_key                  = %q
    endpoints                   = { s3 = "http://host.docker.internal:19000" }
    use_path_style              = true
    skip_credentials_validation = true
    skip_requesting_account_id  = true
    skip_metadata_api_check     = true
    skip_region_validation      = true
  }
}
`, w.bucket, key, os.Getenv("STACKWRIGHT_S3_ACCESS_KEY"), os.Getenv("STACKWRIGHT_S3_SECRET_KEY"))
	const sha = "1111111111111111111111111111111111111111"
	w.github.mu.Lock()
	w.github.heads["main"] = sha
	w.github.mu.Unlock()
	w.github.commit(sha, map[string]string{
		"committed/main.tf":           randomPet,
		"committed/terraform.tfstate": petState("committed-lineage", 3),
		"cli/main.tf":                 randomPet,
		"cli/terraform.tfstate.d/staging/terraform.tfstate": petState("staging-lineage", 2),
		"cli/terraform.tfstate.d/prod/terraform.tfstate":    petState("prod-lineage", 5),
		"remote/backend.tf": backend,
		"remote/main.tf":    randomPet,
	})

	scan := owner.must("ImportService/ScanRepository", map[string]any{"org": "a", "repo": "acme/infra"})
	if scan["commit"] != sha {
		t.Fatalf("scan: %v", scan)
	}
	got := map[string]string{}
	for _, r := range scan["roots"].([]any) {
		root := r.(map[string]any)
		got[root["path"].(string)] = fmt.Sprintf("%v %v %v", root["stateBackend"], root["backend"], root["workspaces"])
	}
	want := map[string]string{
		"committed": "STATE_BACKEND_GIT local [default]",
		"cli":       "STATE_BACKEND_GIT local [prod staging]",
		"remote":    "STATE_BACKEND_CODE s3 [default]",
	}
	for path, w := range want {
		if got[path] != w {
			t.Errorf("root %s: %q, want %q", path, got[path], w)
		}
	}

	roots := []any{}
	for _, path := range []string{"committed", "cli", "remote"} {
		roots = append(roots, map[string]any{"path": path, "slug": "infra-" + path, "engine": "ENGINE_OPENTOFU", "engineVersion": "1.10.7",
			"labels": map[string]string{"repo": "acme/infra", "path": path}})
	}
	imp := owner.must("ImportService/StartImport", map[string]any{"org": "a", "repo": "acme/infra", "commit": sha, "roots": roots})["import"].(map[string]any)
	id := imp["id"].(string)
	if n := len(imp["workspaces"].([]any)); n != 4 {
		t.Fatalf("%d workspaces, want 4: %v", n, imp)
	}

	// Every first plan reads the state where the code keeps it.
	deadline := time.Now().Add(10 * time.Minute)
	for {
		imp = owner.must("ImportService/GetImport", map[string]any{"org": "a", "id": id})["import"].(map[string]any)
		done := 0
		for _, x := range imp["workspaces"].([]any) {
			ws := x.(map[string]any)
			switch ws["runStatus"] {
			case "RUN_STATUS_PLANNED":
				done++
				if ws["hasChanges"] == true {
					t.Errorf("%s/%s: the first plan has changes", ws["project"], ws["workspace"])
				}
			case "RUN_STATUS_FAILED":
				t.Fatalf("%s/%s: the first plan failed: %v", ws["project"], ws["workspace"], ws["runError"])
			}
		}
		if done == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the first plans did not finish: %v", imp)
		}
		time.Sleep(time.Second)
	}

	// The committed-state project cannot apply, and Stackwright keeps no
	// copy of any imported state.
	ctx := context.Background()
	var planOnly, copies int
	if err := w.owner.QueryRow(ctx, `SELECT count(*) FILTER (WHERE plan_only), (SELECT count(*) FROM state_versions) FROM runs`).
		Scan(&planOnly, &copies); err != nil {
		t.Fatal(err)
	}
	if planOnly != 4 || copies != 0 {
		t.Errorf("%d plan-only runs, %d state versions", planOnly, copies)
	}
	if _, err := w.owner.Exec(ctx, `UPDATE runs SET plan_only = false WHERE workspace_id IN (SELECT w.id FROM workspaces w
		JOIN projects p ON p.id = w.project_id WHERE p.slug = 'infra-committed')`); err == nil || !strings.Contains(err.Error(), "only plans") {
		t.Errorf("an apply of committed state: %v", err)
	}
	obj, err := w.s3.GetObject(ctx, w.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var after bytes.Buffer
	if _, err := after.ReadFrom(obj); err != nil {
		t.Fatal(err)
	}
	if after.String() != state {
		t.Errorf("the plan changed the code's backend: %s", after.String())
	}
	req, _ := http.NewRequest(http.MethodGet, w.srv.URL+"/state/a/infra-remote/default", nil)
	req.SetBasicAuth("stackwright", "anything")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Error("Stackwright's state address serves a project on the code's backend")
		}
	}

	// A second import finds everything and creates nothing.
	var before [3]int
	count := func() (n [3]int) {
		if err := w.owner.QueryRow(ctx, `SELECT (SELECT count(*) FROM projects), (SELECT count(*) FROM workspaces), (SELECT count(*) FROM runs)`).
			Scan(&n[0], &n[1], &n[2]); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before = count()
	again := owner.must("ImportService/StartImport", map[string]any{"org": "a", "repo": "acme/infra", "commit": sha, "roots": roots})["import"].(map[string]any)
	for _, x := range again["workspaces"].([]any) {
		if ws := x.(map[string]any); ws["created"] == true || ws["runNumber"] != nil {
			t.Errorf("the second import made %v", ws)
		}
	}
	if after := count(); after != before {
		t.Errorf("projects, workspaces and runs before and after the second import: %v, %v", before, after)
	}
	rescan := owner.must("ImportService/ScanRepository", map[string]any{"org": "a", "repo": "acme/infra"})
	for _, r := range rescan["roots"].([]any) {
		if root := r.(map[string]any); root["existingProject"] != "infra-"+root["path"].(string) {
			t.Errorf("rescan: %v", root)
		}
	}
}
