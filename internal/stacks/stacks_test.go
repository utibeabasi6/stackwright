package stacks

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "write the generated modules' golden files")

func readStack(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if IsStackFile(e.Name()) {
			if files[e.Name()], err = os.ReadFile(filepath.Join(dir, e.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
	return files
}

var env = Env{Tokens: map[string]string{"aws": "token"}, Store: func(category, key string) (string, bool) { return category + ":" + key, true }}

// The examples are HashiCorp's, under MPL-2.0; each folder's SOURCE.txt
// names its repository and commit. They follow Format.
func TestHashiCorpExamples(t *testing.T) {
	want := map[string][]string{
		"eks-deferred-stack": {"development-cluster", "development-kube <- cluster"},
		"gcp-stacks-example": {"production-compute"},
		"lambda-component-expansion-stack": {
			"development-api_gateway-us-east-1 <- lambda", "development-lambda-us-east-1 <- s3", "development-s3-us-east-1",
			"production-api_gateway-us-east-1 <- lambda", "production-api_gateway-us-west-1 <- lambda",
			"production-lambda-us-east-1 <- s3", "production-lambda-us-west-1 <- s3", "production-s3-us-east-1", "production-s3-us-west-1",
		},
		"learn-terraform-stacks-deploy-aws": {
			"development-instance-us-east-1 <- key_pair,vpc", "development-key_pair-us-east-1", "development-vpc-us-east-1",
			"development-outputs <- instance",
			"production-instance-us-east-1 <- key_pair,vpc", "production-instance-us-west-1 <- key_pair,vpc",
			"production-key_pair-us-east-1", "production-key_pair-us-west-1", "production-vpc-us-east-1", "production-vpc-us-west-1",
			"production-outputs <- instance",
		},
		"pet-nulls-stack": {"complex-nulls <- pet", "complex-pet", "simple-nulls <- pet", "simple-pet"},
	}
	golden := map[string]bool{"simple-nulls": true, "production-api_gateway-us-west-1": true, "development-kube": true,
		"development-outputs": true, "production-compute": true}
	for name, instances := range want {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join("testdata", "hashicorp", name)
			c, err := Parse(readStack(t, dir))
			if err != nil {
				t.Fatal(err)
			}
			if len(c.Refused) > 0 {
				t.Errorf("refused %v", c.Refused)
			}
			var got []string
			for _, d := range c.Deployments {
				vals, err := c.Values(d.Name, env)
				if err != nil {
					t.Fatal(err)
				}
				all, err := c.Instances(d.Name, vals, nil)
				if err != nil {
					t.Fatal(err)
				}
				for _, i := range all {
					needs := c.OutputsNeed()
					mod := Module{}
					if i.Component != "" {
						needs = c.Needs(i.Component)
						if mod, err = ReadModule(filepath.Join(dir, c.Components[i.Component].Source)); err != nil {
							t.Fatal(err)
						}
					}
					line := i.Workspace()
					if len(needs) > 0 {
						line += " <- " + strings.Join(needs, ",")
					}
					got = append(got, line)
					g, err := c.Generate(i, vals, nil, false, mod)
					if err != nil {
						t.Fatalf("%s: %v", i.Workspace(), err)
					}
					if !golden[i.Workspace()] {
						continue
					}
					path := filepath.Join("testdata", "golden", i.Workspace()+".golden")
					text := fmt.Sprintf("%s\n# %s\n# env: %s\n", g.Main, strings.ReplaceAll(string(g.Vars), "\n", "\n# "), strings.Join(g.Env, " "))
					if *update {
						if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
							t.Fatal(err)
						}
					}
					if b, err := os.ReadFile(path); err != nil || string(b) != text {
						t.Errorf("%s differs from %s (go test -update writes it):\n%s", i.Workspace(), path, text)
					}
				}
			}
			if !slices.Equal(got, instances) {
				t.Errorf("instances\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(instances, "\n"))
			}
		})
	}
}

const minimal = `
required_providers {
  random = {
    source  = "hashicorp/random"
    version = "3.7.2"
  }
}
variable "names" {
  type = set(string)
}
provider "random" "this" {}
component "pet" {
  source   = "./pet"
  for_each = var.names
  inputs   = { prefix = each.value }
  providers = {
    random = provider.random.this
  }
}
`

func parse(t *testing.T, files map[string]string) *Config {
	t.Helper()
	in := map[string][]byte{}
	for k, v := range files {
		in[k] = []byte(v)
	}
	c, err := Parse(in)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRefusedBlocksNameFileAndLine(t *testing.T) {
	c := parse(t, map[string]string{
		"main.tfcomponent.hcl": minimal + `
stack "child" {
  source = "./child"
}
`,
		"deploy.tfdeploy.hcl": `deployment_group "canary" {
  auto_approve_checks = [deployment_auto_approve.safe]
}
deployment_auto_approve "safe" {
  check {
    condition = context.plan.changes.remove == 0
    reason    = "no deletes"
  }
}
publish_output "x" {
  value = deployment.dev.x
}
store "tfvars" "x" {
  path = "x.tfvars"
}
deployment "dev" {
  inputs           = { names = ["a"] }
  deployment_group = deployment_group.canary
  migrate          = true
}
`,
		"extra.tfdeploy.json": `{}`,
	})
	var got []string
	for _, err := range c.Refused {
		if !errors.Is(err, ErrRefused) {
			t.Errorf("%v is not ErrRefused", err)
		}
		got = append(got, err.Error())
	}
	want := []string{
		"deploy.tfdeploy.hcl:10: a publish_output block is not supported by Stackwright yet",
		`deploy.tfdeploy.hcl:13: store "tfvars" is not supported by Stackwright yet`,
		"deploy.tfdeploy.hcl:19: migrate in a deployment is not supported by Stackwright yet",
		"deploy.tfdeploy.hcl:2: auto_approve_checks in a deployment_group is not supported by Stackwright yet",
		"deploy.tfdeploy.hcl:4: a deployment_auto_approve block is not supported by Stackwright yet",
		"extra.tfdeploy.json:1: a Stacks file in JSON is not supported by Stackwright yet",
		"main.tfcomponent.hcl:21: an embedded stack block is not supported by Stackwright yet",
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("refused\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestForEach(t *testing.T) {
	for _, tc := range []struct {
		names, err string
		want       []string
	}{
		{names: `["b", "a"]`, want: []string{"dev-pet-a", "dev-pet-b"}},
		{names: `[]`},
		{names: `null`, err: `deployment "dev" has no value for variable "names"`},
	} {
		c := parse(t, map[string]string{"main.tfcomponent.hcl": minimal,
			"d.tfdeploy.hcl": fmt.Sprintf("deployment \"dev\" {\n  inputs = { names = %s }\n}\n", tc.names)})
		vals, err := c.Values("dev", Env{})
		if err == nil {
			var all []Instance
			all, err = c.Instances("dev", vals, nil)
			var got []string
			for _, i := range all {
				got = append(got, i.Workspace())
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("%s: instances %v, want %v", tc.names, got, tc.want)
			}
		}
		if (err != nil) != (tc.err != "") || err != nil && !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%s: error %v, want %q", tc.names, err, tc.err)
		}
	}

	c := parse(t, map[string]string{"main.tfcomponent.hcl": minimal + `
component "copies" {
  source   = "./pet"
  for_each = component.pet["a"].ids
}
component "listed" {
  source   = "./pet"
  for_each = tolist(var.names)
}
`, "d.tfdeploy.hcl": "deployment \"dev\" {\n  inputs = { names = [\"a\"] }\n}\n"})
	vals, err := c.Values("dev", Env{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Instances("dev", vals, nil)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "main.tfcomponent.hcl:23: a for_each that is not known before an apply, in component copies, is not supported") {
		t.Errorf("unknown for_each: %v", err)
	}
	delete(c.Components, "copies")
	if _, err = c.Instances("dev", vals, nil); err == nil || !strings.Contains(err.Error(), "must be a map or a set of strings") {
		t.Errorf("list for_each: %v", err)
	}
}

func TestRemovedInstances(t *testing.T) {
	deploy := "deployment \"dev\" {\n  inputs = { names = [\"a\"] }\n}\n"
	prior := []Prior{{Component: "pet", Key: "a", Keyed: true}, {Component: "pet", Key: "b", Keyed: true}, {Component: "old"}}
	c := parse(t, map[string]string{"main.tfcomponent.hcl": minimal, "d.tfdeploy.hcl": deploy})
	vals, err := c.Values("dev", Env{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Instances("dev", vals, prior); err == nil || !strings.Contains(err.Error(), `component pet["b"] is gone from the files with no removed block`) {
		t.Errorf("gone without removed: %v", err)
	}

	c = parse(t, map[string]string{"main.tfcomponent.hcl": minimal + `
removed {
  from     = component.pet[each.key]
  for_each = toset(["b"])
  source   = "./pet"
  providers = {
    random = provider.random.this
  }
}
removed {
  from   = component.old
  source = "./old"
  lifecycle {
    destroy = false
  }
}
`, "d.tfdeploy.hcl": deploy})
	all, err := c.Instances("dev", vals, prior)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, i := range all {
		got = append(got, fmt.Sprintf("%s removed=%t destroy=%t", i.Workspace(), i.Removed, i.Destroy))
	}
	want := []string{"dev-pet-a removed=false destroy=false", "dev-pet-b removed=true destroy=true", "dev-old removed=true destroy=false"}
	if !slices.Equal(got, want) {
		t.Errorf("instances %v, want %v", got, want)
	}
	g, err := c.Generate(all[1], vals, nil, false, Module{Variables: map[string]bool{"prefix": false, "length": true}, Outputs: map[string]bool{"name": false}})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`source = "./pet"`, "prefix = null", "random = random.this_0"} {
		if !strings.Contains(string(g.Main), s) {
			t.Errorf("the removed instance's module lacks %q:\n%s", s, g.Main)
		}
	}
	if strings.Contains(string(g.Main), "output ") || strings.Contains(string(g.Main), "length") {
		t.Errorf("the removed instance's module has outputs or an input with a default:\n%s", g.Main)
	}
}

func TestDeploymentValues(t *testing.T) {
	c := parse(t, map[string]string{"main.tfcomponent.hcl": `
variable "token" {
  type      = string
  ephemeral = true
}
variable "key" {
  type = string
}
variable "tags" {
  type    = object({ team = string, cost = optional(string, "none") })
  default = { team = "infra" }
}
`, "d.tfdeploy.hcl": `
identity_token "aws" {
  audience = ["sts.amazonaws.com"]
}
store "varset" "creds" {
  name     = "creds"
  category = "env"
}
locals {
  prefix = "k-"
}
deployment "dev" {
  inputs = {
    token = identity_token.aws.jwt
    key   = "${local.prefix}${store.varset.creds.stable.API_KEY}"
  }
}
deployment "bad" {
  inputs = {
    token = "x"
    key   = "y"
    typo  = 1
  }
}
`})
	if aud, err := c.Audiences(); err != nil || !slices.Equal(aud["aws"], []string{"sts.amazonaws.com"}) {
		t.Errorf("audiences %v, %v", aud, err)
	}
	vals, err := c.Values("dev", env)
	if err != nil {
		t.Fatal(err)
	}
	if vals["token"].AsString() != "token" || vals["key"].AsString() != "k-env:API_KEY" ||
		vals["tags"].GetAttr("cost").AsString() != "none" {
		t.Errorf("values %#v", vals)
	}
	if vals, err := c.Values("dev", Env{}); err != nil || vals["token"].IsKnown() {
		t.Errorf("a sync's values: the token should be unknown: %v %v", vals, err)
	}
	if _, err := c.Values("bad", env); err == nil || !strings.Contains(err.Error(), `d.tfdeploy.hcl:22: deployment "bad" sets "typo", which is not a stack variable`) {
		t.Errorf("an undeclared input: %v", err)
	}
	g, err := c.Generate(Instance{Deployment: "dev"}, vals, nil, false, Module{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(g.Env, []string{"TF_VAR_token=token"}) || strings.Contains(string(g.Vars), "token") {
		t.Errorf("the ephemeral token must go only in the environment: env %v, vars %s", g.Env, g.Vars)
	}
}

func TestWorkspaceNames(t *testing.T) {
	for _, tc := range []struct {
		i    Instance
		want string
	}{
		{Instance{Deployment: "Prod", Component: "vpc"}, "prod-vpc"},
		{Instance{Deployment: "prod", Component: "api_gateway", Key: "us-east-1", Keyed: true}, "prod-api_gateway-us-east-1"},
		{Instance{Deployment: "prod", Component: "bucket", Key: "Logs/Archive", Keyed: true}, "prod-bucket-logs-archive"},
		{Instance{Deployment: "prod"}, "prod-outputs"},
	} {
		if got := tc.i.Workspace(); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.i, got, tc.want)
		}
	}
}
