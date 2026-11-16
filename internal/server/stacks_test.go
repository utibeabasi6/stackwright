package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// stackRepo is a Terraform Stack: network, then app for each of two
// names, which reads network's output; dev and prod deploy it. With
// token, the Stack sends an identity token to an ephemeral variable that
// checks it is a JWT.
func stackRepo(token bool, deployExtra string) map[string]string {
	components := `required_providers {
  random = {
    source  = "hashicorp/random"
    version = "3.7.2"
  }
}

variable "env" {
  type = string
}

provider "random" "this" {}

component "network" {
  source = "./network"
  inputs = {
    name = var.env
  }
  providers = {
    random = provider.random.this
  }
}

component "app" {
  source   = "./app"
  for_each = toset(["api", "web"])
  inputs = {
    name   = each.key
    vpc_id = component.network.vpc_id
  }
}

output "apps" {
  type  = map(string)
  value = { for k, a in component.app : k => a.seen }
}
`
	deploy := "deployment \"dev\" {\n  inputs = { env = \"dev\" }\n}\n\ndeployment \"prod\" {\n  inputs = { env = \"prod\" }\n}\n"
	if token {
		components += `
variable "token" {
  type      = string
  ephemeral = true
  validation {
    condition     = length(split(".", var.token)) == 3
    error_message = "The token is not a JWT."
  }
}
`
		deploy = `identity_token "test" {
  audience = ["stackwright-test"]
}

deployment "dev" {
  inputs = {
    env   = "dev"
    token = identity_token.test.jwt
  }
}

deployment "prod" {
  inputs = {
    env   = "prod"
    token = identity_token.test.jwt
  }
}
`
	}
	return map[string]string{
		"stack/components.tfcomponent.hcl": components,
		"stack/deployments.tfdeploy.hcl":   deploy + deployExtra,
		"stack/network/main.tf":            "variable \"name\" {}\n\nresource \"random_pet\" \"this\" {\n  prefix = var.name\n}\n\noutput \"vpc_id\" {\n  value = \"vpc-${random_pet.this.id}\"\n}\n",
		"stack/network/versions.tf":        "terraform {\n  required_providers {\n    random = { source = \"hashicorp/random\" }\n  }\n}\n",
		"stack/app/main.tf":                "variable \"name\" {}\nvariable \"vpc_id\" {}\n\nresource \"terraform_data\" \"this\" {\n  input = \"${var.name} in ${var.vpc_id}\"\n}\n\noutput \"seen\" {\n  value = terraform_data.this.output\n}\n",
	}
}

func TestStackDeploysEachDeploymentInOrderWithOneApproval(t *testing.T) {
	for _, engine := range []struct{ name, engine, version string }{
		{"opentofu", "ENGINE_OPENTOFU", "1.12.8"},
		{"terraform", "ENGINE_TERRAFORM", "1.5.7"},
	} {
		t.Run(engine.name, func(t *testing.T) {
			t.Parallel()
			requireDocker(t)
			w := start(t, nil, true)
			owner := w.setup()
			rw := &runWorld{world: w, owner: owner, reviewer: w.member(owner, "a", "reviewer@a.test", "ROLE_REVIEWER")}
			w.createApp(owner)
			w.link(owner)
			w.startBuilder(4)
			// Terraform 1.5.7 has no ephemeral variables, so only OpenTofu
			// gets the identity token.
			token := engine.name == "opentofu"
			w.github.mu.Lock()
			w.github.heads["main"] = shaBase
			w.github.mu.Unlock()
			w.github.commit(shaBase, stackRepo(token, ""))

			scan := owner.must("ImportService/ScanRepository", map[string]any{"org": "a", "repo": "acme/infra"})
			var found map[string]any
			for _, r := range asList(scan["roots"]) {
				if root := r.(map[string]any); root["path"] == "stack" {
					found = root
				} else {
					t.Errorf("a root other than the Stack: %v", root)
				}
			}
			if found == nil || found["stateBackend"] != "STATE_BACKEND_STACKWRIGHT" || fmt.Sprint(found["stackDeployments"]) != "[dev prod]" {
				t.Fatalf("the Stack's root: %v", found)
			}
			owner.must("ImportService/StartImport", map[string]any{"org": "a", "repo": "acme/infra", "commit": shaBase,
				"roots": []any{map[string]any{"path": "stack", "slug": "shop", "engine": engine.engine, "engineVersion": engine.version}}})

			var p map[string]any
			for _, x := range asList(owner.must("ProjectService/ListProjects", map[string]any{"org": "a"})["projects"]) {
				if x.(map[string]any)["slug"] == "shop" {
					p = x.(map[string]any)
				}
			}
			if p == nil {
				t.Fatal("no project shop")
			}
			var names []string
			for _, ws := range asList(p["workspaces"]) {
				names = append(names, ws.(map[string]any)["name"].(string))
			}
			slices.Sort(names)
			want := []string{"dev-app-api", "dev-app-web", "dev-network", "dev-outputs", "prod-app-api", "prod-app-web", "prod-network", "prod-outputs"}
			if p["layout"] != "LAYOUT_STACKS" || !slices.Equal(names, want) {
				t.Fatalf("project %v, workspaces %v", p["layout"], names)
			}
			if _, code := owner.call("DeploymentService/SaveDeployment", map[string]any{"org": "a", "name": "shop-dev",
				"steps": []any{map[string]any{"project": "shop", "workspace": "dev-network"}}}); code != "failed_precondition" {
				t.Errorf("an edit of a Stack's deployment: %q", code)
			}

			for _, dep := range []string{"shop-dev", "shop-prod"} {
				run := rw.waitDeployment(dep, 1, "DEPLOYMENT_RUN_STATUS_NEEDS_APPROVAL")
				var order []string
				for _, s := range asList(run["steps"]) {
					s := s.(map[string]any)
					order = append(order, s["workspace"].(map[string]any)["workspace"].(string))
					if planned := s["runNumber"] != nil; planned != strings.HasSuffix(order[len(order)-1], "-network") {
						t.Errorf("%s: step %v planned before what it reads applied, or network did not", dep, s)
					}
				}
				env := strings.TrimPrefix(dep, "shop-")
				if strings.Join(order, ",") != fmt.Sprintf("%[1]s-network,%[1]s-app-api,%[1]s-app-web,%[1]s-outputs", env) {
					t.Errorf("%s: order %v", dep, order)
				}
				rw.reviewer.must("DeploymentService/ApproveDeploymentRun", map[string]any{"org": "a", "name": dep, "number": 1})
				rw.waitDeployment(dep, 1, "DEPLOYMENT_RUN_STATUS_APPLIED")

				// The stack outputs hold each app's view of its network.
				var orgID string
				var sealed []byte
				if err := w.owner.QueryRow(context.Background(), `SELECT o.org_id, o.outputs FROM workspace_outputs o
					JOIN workspaces w ON w.id = o.workspace_id WHERE w.name = $1`, env+"-outputs").Scan(&orgID, &sealed); err != nil {
					t.Fatal(err)
				}
				tx, err := w.owner.Begin(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				plain, err := w.runs.KEK.Open(context.Background(), tx, orgID, sealed)
				_ = tx.Rollback(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				var outs map[string]struct {
					Value map[string]string `json:"value"`
				}
				if err := json.Unmarshal(plain, &outs); err != nil {
					t.Fatal(err)
				}
				apps := outs["apps"].Value
				if !strings.HasPrefix(apps["api"], "api in vpc-"+env+"-") || !strings.HasPrefix(apps["web"], "web in vpc-"+env+"-") {
					t.Errorf("%s: the stack outputs %v", dep, apps)
				}
			}
			if !token {
				return
			}

			// A push that adds a block Stackwright refuses fails the plan
			// with its name, file and line.
			w.github.commit(shaHead, stackRepo(token, "\ndeployment_auto_approve \"safe\" {\n  check {\n    condition = true\n    reason    = \"x\"\n  }\n}\n"))
			w.github.diff(shaBase, shaHead, "stack/deployments.tfdeploy.hcl")
			w.github.deliver(w, "push", "stack-push", map[string]any{"ref": "refs/heads/main", "before": shaBase, "after": shaHead,
				"installation": map[string]any{"id": 7}, "repository": map[string]any{"id": 70, "full_name": "acme/infra"}})
			eventually(t, "a deployment run of the push", func() bool {
				return deploymentRun(owner, "shop-dev", 2) != nil
			})
			failed := rw.waitDeployment("shop-dev", 2, "DEPLOYMENT_RUN_STATUS_FAILED")
			if msg := failed["error"].(string); !strings.Contains(msg,
				"deployments.tfdeploy.hcl:19: a deployment_auto_approve block is not supported by Stackwright yet") {
				t.Errorf("the refused block: %s", msg)
			}

			// A removed block destroys the instance it names, after every
			// other step; the next sync archives its workspace.
			removed := stackRepo(token, "")
			removed["stack/components.tfcomponent.hcl"] = strings.Replace(removed["stack/components.tfcomponent.hcl"], `toset(["api", "web"])`,
				`toset(["api"])`, 1) + "\nremoved {\n  from     = component.app[each.key]\n  for_each = toset([\"web\"])\n  source   = \"./app\"\n}\n"
			w.github.commit(shaMerge, removed)
			w.github.diff(shaHead, shaMerge, "stack/components.tfcomponent.hcl")
			w.github.deliver(w, "push", "stack-remove", map[string]any{"ref": "refs/heads/main", "before": shaHead, "after": shaMerge,
				"installation": map[string]any{"id": 7}, "repository": map[string]any{"id": 70, "full_name": "acme/infra"}})
			eventually(t, "a deployment run that removes web", func() bool {
				return deploymentRun(owner, "shop-dev", 3) != nil
			})
			run := rw.waitDeployment("shop-dev", 3, "DEPLOYMENT_RUN_STATUS_NEEDS_APPROVAL")
			steps := asList(run["steps"])
			if last := steps[len(steps)-1].(map[string]any)["workspace"].(map[string]any)["workspace"]; last != "dev-app-web" {
				t.Errorf("the removed instance is not the last step: %v", steps)
			}
			rw.reviewer.must("DeploymentService/ApproveDeploymentRun", map[string]any{"org": "a", "name": "shop-dev", "number": 3})
			rw.waitDeployment("shop-dev", 3, "DEPLOYMENT_RUN_STATUS_APPLIED")
			var destroy, archived bool
			var status string
			if err := w.owner.QueryRow(context.Background(), `SELECT r.destroy, r.status, w.archived_at IS NOT NULL FROM runs r
				JOIN workspaces w ON w.id = r.workspace_id WHERE w.name = 'dev-app-web' ORDER BY r.number DESC LIMIT 1`).
				Scan(&destroy, &status, &archived); err != nil || !destroy || status != "applied" || !archived {
				t.Errorf("dev-app-web: destroy %t, %s, archived %t: %v", destroy, status, archived, err)
			}
		})
	}
}
