package findings

import (
	"os"
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

func TestChangedListsWhatAPlanCreatesOrUpdates(t *testing.T) {
	raw, err := os.ReadFile("testdata/plan.json")
	if err != nil {
		t.Fatal(err)
	}
	changed, err := Changed(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !changed["aws_security_group.ssh"] || !changed["aws_s3_bucket.logs"] || len(changed) != 5 {
		t.Errorf("changed: %v", changed)
	}
	none, _ := Changed([]byte(`{"resource_changes": [{"address": "a.b", "change": {"actions": ["no-op"]}}, {"address": "c.d", "change": {"actions": ["delete"]}}]}`))
	if len(none) != 0 {
		t.Errorf("no-op and delete count as changes: %v", none)
	}
}
