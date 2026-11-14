package findings

import (
	"os"
	"slices"
	"testing"
)

func TestParseReadsTrivysReport(t *testing.T) {
	raw, err := os.ReadFile("testdata/scan.json")
	if err != nil {
		t.Fatal(err)
	}
	results, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 10 {
		t.Errorf("%d results, want 10: %+v", len(results), results)
	}
	var ssh *Result
	for i, r := range results {
		if r.CheckID == "AWS-0107" {
			ssh = &results[i]
		}
	}
	if ssh == nil || ssh.Resource != "aws_security_group.ssh" || ssh.Severity != "high" || !Blocks(ssh.Severity) {
		t.Errorf("the SSH finding: %+v", ssh)
	}
	if _, err := Parse([]byte("not json")); err == nil {
		t.Error("a broken report parsed")
	}
}

func TestMatchListsWhatAPlanChanges(t *testing.T) {
	raw, err := os.ReadFile("testdata/plan.json")
	if err != nil {
		t.Fatal(err)
	}
	_, changed, err := Match(nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !changed["aws_security_group.ssh"] || !changed["aws_s3_bucket.logs"] || len(changed) != 5 {
		t.Errorf("changed %v", changed)
	}
	results, changed, _ := Match([]Result{{Resource: "aws_subnet.public"}, {Resource: "a.b"}}, []byte(`{"resource_changes": [
		{"mode": "managed", "type": "a", "name": "b", "change": {"actions": ["no-op"]}},
		{"mode": "managed", "type": "c", "name": "d", "change": {"actions": ["delete"]}},
		{"mode": "managed", "type": "aws_subnet", "name": "public", "index": 0, "change": {"actions": ["create"]}},
		{"mode": "data", "type": "aws_region", "name": "here", "change": {"actions": ["read"]}}]}`))
	if len(changed) != 1 || !changed["aws_subnet.public"] || results[0].Unmatched || results[1].Unmatched {
		t.Errorf("results %+v, changed %v", results, changed)
	}
}

// The fixture is OpenTofu 1.10.7 and Trivy 0.76.1 output for
// testdata/modules: one root security group, and six in plain, count and
// for_each modules.
func TestMatchFindsModuleResourcesByTrivysHash(t *testing.T) {
	plan, err := os.ReadFile("testdata/modules/plan.json")
	if err != nil {
		t.Fatal(err)
	}
	scan, err := os.ReadFile("testdata/modules/scan.json")
	if err != nil {
		t.Fatal(err)
	}
	results, err := Parse(scan)
	if err != nil {
		t.Fatal(err)
	}
	results, changed, err := Match(append(results, Result{CheckID: "AWS-0104", Resource: "aws_security_group.this_00000000000000000000000000000000"}), plan)
	if err != nil {
		t.Fatal(err)
	}
	var got, unmatched []string
	for _, r := range results {
		if r.CheckID != "AWS-0104" {
			continue
		}
		if r.Unmatched {
			unmatched = append(unmatched, r.Resource)
			continue
		}
		got = append(got, r.Resource)
		if !changed[r.Resource] {
			t.Errorf("%s is not a change", r.Resource)
		}
	}
	slices.Sort(got)
	want := []string{"aws_security_group.root", `module.app.aws_security_group.this`, `module.keyed["a"].aws_security_group.this`,
		`module.keyed["b"].aws_security_group.this`, "module.many[0].aws_security_group.this", "module.many[1].aws_security_group.this",
		"module.web.aws_security_group.this"}
	if !slices.Equal(got, want) || !slices.Equal(unmatched, []string{"aws_security_group.this_00000000000000000000000000000000"}) {
		t.Errorf("matched %v\nunmatched %v", got, unmatched)
	}
}
