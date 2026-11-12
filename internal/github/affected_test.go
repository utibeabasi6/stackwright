package github

import (
	"slices"
	"testing"
)

func TestAffectedFollowsLocalModules(t *testing.T) {
	files := map[string][]byte{
		"network/main.tf":          []byte(`module "vpc" { source = "../modules/vpc" }`),
		"app/main.tofu":            []byte(`module "svc" { source = "../modules/svc" }`),
		"modules/svc/main.tf":      []byte(`module "inner" { source = "./inner" }`),
		"modules/svc/inner/x.tf":   []byte(`output "x" { value = 1 }`),
		"modules/vpc/main.tf.json": []byte(`{"module": {"subnets": {"source": "../subnets"}}}`),
		"modules/subnets/main.tf":  []byte(`output "x" { value = 1 }`),
		"broken/main.tf":           []byte(`module "x" {`),
	}
	projects := map[string]string{"network": "network", "app": "app", "broken": "broken", "root": "."}
	for _, c := range []struct {
		changed []string
		want    []string
	}{
		{[]string{"network/variables.tf"}, []string{"network", "root"}},
		{[]string{"modules/subnets/main.tf"}, []string{"network", "root"}},
		{[]string{"modules/svc/inner/x.tf"}, []string{"app", "root"}},
		{[]string{"README.md"}, []string{"root"}},
		{[]string{"broken/main.tf"}, []string{"broken", "root"}},
		{nil, []string{"app", "broken", "network", "root"}},
	} {
		got := affected(projects, c.changed, files)
		var ids []string
		for id, ok := range got {
			if ok {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		if !slices.Equal(ids, c.want) {
			t.Errorf("%v: touched %v, want %v", c.changed, ids, c.want)
		}
	}
}
