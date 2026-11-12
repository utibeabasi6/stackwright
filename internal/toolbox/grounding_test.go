package toolbox

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func contains(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
}

func TestConventionsReadsRulesAndFacts(t *testing.T) {
	repo := t.TempDir()
	writeFiles(t, repo, map[string]string{
		"STACKWRIGHT.md":            "Tag every resource with team = \"platform\".",
		"infra/STACKWRIGHT.md":      "Buckets end in -logs.",
		"infra/.env":                "SECRET=1",
		"infra/notes.txt":           "not code",
		"infra/modules/net/main.tf": "",
		"infra/main.tf": `terraform {
  required_version = ">= 1.8"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
  backend "s3" {}
}

module "net" {
  source = "./modules/net"
}
`,
		"infra/.terraform.lock.hcl": `provider "registry.opentofu.org/hashicorp/aws" {
  version     = "5.80.0"
  constraints = "~> 5.0"
}
`,
	})
	b := &box{repo: repo, dir: filepath.Join(repo, "infra")}
	out, err := b.conventions()
	if err != nil {
		t.Fatal(err)
	}
	contains(t, out,
		"STACKWRIGHT.md:\nTag every resource",
		"infra/STACKWRIGHT.md:\nBuckets end in -logs.",
		"Files: main.tf, modules/",
		`required_version = ">= 1.8" (main.tf)`,
		`Provider aws = { source = "hashicorp/aws" version = "~> 5.0" } (main.tf)`,
		"Backend: s3 (main.tf)",
		`Module net from "./modules/net" (main.tf)`,
		"Locked: registry.opentofu.org/hashicorp/aws 5.80.0",
	)
	if strings.Contains(out, ".env") || strings.Contains(out, "SECRET") || strings.Contains(out, "notes.txt") {
		t.Errorf("conventions show a file they should not:\n%s", out)
	}
}

func TestConventionsRefusesASymlinkOutOfTheCheckout(t *testing.T) {
	outside := t.TempDir()
	writeFiles(t, outside, map[string]string{"secret.md": "do not read"})
	repo := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(repo, "STACKWRIGHT.md")); err != nil {
		t.Fatal(err)
	}
	b := &box{repo: repo, dir: repo}
	out, err := b.conventions()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "do not read") {
		t.Fatalf("read through a symlink out of the checkout:\n%s", out)
	}
}

func TestDescribeModuleListsInputsAndOutputs(t *testing.T) {
	repo := t.TempDir()
	writeFiles(t, repo, map[string]string{"m/variables.tf": `
variable "name" {
  type        = string
  description = "The bucket's name.\nMore text."
}
variable "tags" {
  type    = map(string)
  default = {}
}
variable "password" {
  type      = string
  sensitive = true
}
output "arn" {
  value       = "x"
  description = "The bucket's ARN."
}
`, "m/broken.tf": `variable "x" {`})
	root, err := os.OpenRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	files, err := tfFiles(root, "m")
	if err != nil {
		t.Fatal(err)
	}
	var w strings.Builder
	describeModule(&w, files)
	want := `Inputs:
  name: string, required — The bucket's name.
  password: string, required, sensitive
  tags: map(string), default {}
Outputs:
  arn — The bucket's ARN.
`
	if w.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", w.String(), want)
	}
}

const schemaJSON = `{"format_version":"1.0","provider_schemas":{"registry.opentofu.org/hashicorp/aws":{
"resource_schemas":{
  "aws_s3_bucket":{"block":{"attributes":{
    "bucket":{"type":"string","optional":true,"computed":true,"description":"The bucket's name."},
    "arn":{"type":"string","computed":true},
    "acl":{"type":"string","optional":true,"deprecated":true},
    "tags":{"type":["map","string"],"optional":true}},
  "block_types":{"versioning":{"nesting_mode":"list","max_items":1,"block":{"deprecated":true,"attributes":{"enabled":{"type":"bool","optional":true}}}}}}},
  "aws_s3_bucket_versioning":{"block":{"attributes":{"bucket":{"type":"string","required":true}},
  "block_types":{"versioning_configuration":{"nesting_mode":"list","min_items":1,"max_items":1,"block":{"attributes":{"status":{"type":"string","required":true}}}}}}},
  "aws_db_instance":{"block":{"attributes":{"password":{"type":"string","optional":true,"sensitive":true},
    "rules":{"nested_type":{"nesting_mode":"set","attributes":{"port":{"type":"number","required":true}}},"optional":true},
    "meta":{"type":["object",{"b":"number","a":["list","dynamic"]}],"optional":true}}}}
},
"data_source_schemas":{"aws_region":{"block":{"attributes":{"name":{"type":"string","computed":true}}}}}}}}`

func TestDescribeTypeWritesTheSchemaAsText(t *testing.T) {
	var f schemaFile
	if err := json.Unmarshal([]byte(schemaJSON), &f); err != nil {
		t.Fatal(err)
	}
	out, ok := describeType(&f, "aws_s3_bucket", false)
	want := `resource aws_s3_bucket, from registry.opentofu.org/hashicorp/aws
acl: string, optional, deprecated
bucket: string, optional — The bucket's name.
tags: map(string), optional
arn: string, read-only
block versioning (list, at most 1, deprecated)
  enabled: bool, optional
`
	if !ok || out != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
	out, _ = describeType(&f, "aws_db_instance", false)
	contains(t, out, "password: string, optional, sensitive", "rules: set of objects, optional\n  port: number, required",
		"meta: object({a = list(any), b = number}), optional")
	out, _ = describeType(&f, "aws_region", true)
	contains(t, out, "data source aws_region", "name: string, read-only")

	out, ok = describeType(&f, "aws_s3_bucket_versionings", false)
	if ok {
		t.Fatal("found a type that does not exist")
	}
	contains(t, out, "No installed provider has a resource aws_s3_bucket_versionings.",
		"Similar types: aws_s3_bucket_versioning, aws_s3_bucket.")
}

// TestGroundingWithARealEngine runs init and the schema command of a local
// OpenTofu or Terraform, which downloads the random provider.
func TestGroundingWithARealEngine(t *testing.T) {
	if os.Getenv("STACKWRIGHT_TEST_ENGINES") == "" {
		t.Skip("set STACKWRIGHT_TEST_ENGINES=1 to run against a real engine")
	}
	engine, err := exec.LookPath("tofu")
	if err != nil {
		if engine, err = exec.LookPath("terraform"); err != nil {
			t.Skip("no tofu or terraform on PATH")
		}
	}
	work := t.TempDir()
	repo := filepath.Join(work, "repo")
	writeFiles(t, repo, map[string]string{
		"main.tf": `terraform {
  required_providers {
    random = { source = "hashicorp/random" }
  }
}
module "names" {
  source = "./modules/names"
  prefix = "x"
}
`,
		"modules/names/main.tf": `variable "prefix" {
  type        = string
  description = "Goes before each name."
}
resource "random_pet" "this" { prefix = var.prefix }
output "name" { value = random_pet.this.id }
`,
	})
	b := &box{work: work, repo: repo, dir: repo, engine: engine}
	ctx := context.Background()

	out, err := b.providerSchema(ctx, "random_password", "")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	contains(t, out, "resource random_password, from registry.", "length: number, required", "result: string, read-only, sensitive")
	if _, err := os.Stat(filepath.Join(work, "schema.json")); err != nil {
		t.Errorf("the schemas were not kept: %v", err)
	}
	out, err = b.providerSchema(ctx, "random_pets", "")
	if err != nil {
		t.Fatal(err)
	}
	contains(t, out, "No installed provider has a resource random_pets. Similar types: random_pet.")

	out, err = b.moduleDocs(ctx, "names")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	contains(t, out, `module "names": ./modules/names`, "prefix: string, required — Goes before each name.", "Outputs:\n  name")
	out, err = b.moduleDocs(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	contains(t, out, "Module calls: names (./modules/names)")
}
