package topology

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	v1 "github.com/utibeabasi6/stackwright/gen/stackwright/v1"
)

var update = flag.Bool("update", false, "write topology.json from the stored plans")

// The plans are OpenTofu 1.10.7 output for testdata/*/main.tf, with
// network's hand-written state, from plan -refresh=false: the fake keys
// reach no AWS API.
func fixture(t *testing.T) *v1.Topology {
	t.Helper()
	var plans []Plan
	for _, ws := range []string{"app", "network"} {
		b, err := os.ReadFile(filepath.Join("testdata", ws, "plan.json"))
		if err != nil {
			t.Fatal(err)
		}
		plans = append(plans, Plan{Project: ws, Workspace: "prod", JSON: b})
	}
	topo, err := Build(plans)
	if err != nil {
		t.Fatal(err)
	}
	return topo
}

func TestGolden(t *testing.T) {
	full := fixture(t)
	for _, project := range []string{"app", "network"} {
		b, err := protojson.Marshal(Scope(full, project))
		if err != nil {
			t.Fatal(err)
		}
		var v any
		_ = json.Unmarshal(b, &v)
		got, _ := json.MarshalIndent(v, "", "  ")
		got = append(got, '\n')
		path := filepath.Join("testdata", "topology-"+project+".json")
		if *update {
			if err := os.WriteFile(path, got, 0o600); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if want, _ := os.ReadFile(path); !bytes.Equal(got, want) {
			t.Errorf("%s differs from %s:\n%s", project, path, got)
		}
	}
}

func TestScopeAddsWhatAProjectUsesAsContext(t *testing.T) {
	full := fixture(t)
	roles := func(topo *v1.Topology) (own, context []string) {
		for _, n := range topo.Nodes {
			if n.Context {
				context = append(context, n.Id)
			} else {
				own = append(own, n.Id)
			}
		}
		return own, context
	}
	own, context := roles(Scope(full, "app"))
	wantOwn := []string{"app/prod/aws_autoscaling_group.app", "app/prod/aws_db_instance.main", "app/prod/aws_lb.web",
		"app/prod/aws_security_group.app", "app/prod/aws_security_group.db", "app/prod/aws_security_group.web", "internet"}
	wantContext := []string{"network/prod/aws_internet_gateway.main", "network/prod/aws_nat_gateway.main", "network/prod/aws_subnet.private[0]",
		"network/prod/aws_subnet.private[1]", "network/prod/aws_subnet.public[0]", "network/prod/aws_subnet.public[1]", "network/prod/aws_vpc.main"}
	if !slices.Equal(own, wantOwn) || !slices.Equal(context, wantContext) {
		t.Errorf("app: own %v\ncontext %v", own, context)
	}
	own, context = roles(Scope(full, "network"))
	if len(context) != 0 || slices.ContainsFunc(own, func(id string) bool { return !strings.HasPrefix(id, "network/") }) {
		t.Errorf("network: own %v\ncontext %v", own, context)
	}
}

func TestTwoZoneVPC(t *testing.T) {
	topo := fixture(t)
	nodes := map[string]*v1.TopologyNode{}
	for _, n := range topo.Nodes {
		nodes[n.Id] = n
	}
	node := func(id string) *v1.TopologyNode {
		t.Helper()
		n := nodes[id]
		if n == nil {
			t.Fatalf("no node %s", id)
		}
		return n
	}
	edge := func(from, to string) *v1.TopologyEdge {
		t.Helper()
		for _, e := range topo.Edges {
			if e.From == from && e.To == to {
				return e
			}
		}
		t.Fatalf("no edge %s -> %s", from, to)
		return nil
	}
	const vpc, igw, nat = "network/prod/aws_vpc.main", "network/prod/aws_internet_gateway.main", "network/prod/aws_nat_gateway.main"
	for _, tc := range []struct {
		subnet, zone, cidr, target string
		public                     bool
	}{
		{"network/prod/aws_subnet.public[0]", "eu-west-1a", "10.0.0.0/24", igw, true},
		{"network/prod/aws_subnet.public[1]", "eu-west-1b", "10.0.1.0/24", igw, true},
		{"network/prod/aws_subnet.private[0]", "eu-west-1a", "10.0.10.0/24", nat, false},
		{"network/prod/aws_subnet.private[1]", "eu-west-1b", "10.0.11.0/24", nat, false},
	} {
		s := node(tc.subnet)
		if s.Vpc != vpc || s.Zone != tc.zone || s.Cidr != tc.cidr || s.Public != tc.public {
			t.Errorf("%s: vpc %s, zone %s, cidr %s, public %v", tc.subnet, s.Vpc, s.Zone, s.Cidr, s.Public)
		}
		if len(s.Routes) != 1 || s.Routes[0].Destination != "0.0.0.0/0" || s.Routes[0].Target != tc.target {
			t.Errorf("%s routes: %v", tc.subnet, s.Routes)
		}
		edge(tc.subnet, tc.target)
	}
	n := node(nat)
	if n.Action != v1.PlanAction_PLAN_ACTION_CREATE || n.AwsId != "" || !slices.Equal(n.Subnets, []string{"network/prod/aws_subnet.public[0]"}) ||
		n.Zone != "eu-west-1a" || n.Vpc != vpc {
		t.Errorf("the planned NAT: %v", n)
	}
	if e := edge("network/prod/aws_subnet.private[0]", nat); e.Action != v1.PlanAction_PLAN_ACTION_CREATE {
		t.Errorf("the route to the planned NAT is %v", e.Action)
	}
	if e := edge("network/prod/aws_subnet.public[0]", igw); e.Action != v1.PlanAction_PLAN_ACTION_NO_OP {
		t.Errorf("the existing route is %v", e.Action)
	}

	public := []string{"network/prod/aws_subnet.public[0]", "network/prod/aws_subnet.public[1]"}
	private := []string{"network/prod/aws_subnet.private[0]", "network/prod/aws_subnet.private[1]"}
	for _, tc := range []struct {
		id      string
		subnets []string
		groups  []string
	}{
		{"app/prod/aws_lb.web", public, []string{"app/prod/aws_security_group.web"}},
		{"app/prod/aws_autoscaling_group.app", private, []string{"app/prod/aws_security_group.app"}},
		{"app/prod/aws_db_instance.main", private, []string{"app/prod/aws_security_group.db"}},
	} {
		m := node(tc.id)
		if !slices.Equal(m.Subnets, tc.subnets) || !slices.Equal(m.SecurityGroups, tc.groups) || m.Vpc != vpc || m.Zone != "" {
			t.Errorf("%s: subnets %v, groups %v, vpc %s, zone %q", tc.id, m.Subnets, m.SecurityGroups, m.Vpc, m.Zone)
		}
	}
	for _, tc := range []struct{ from, to, label string }{
		{"internet", "app/prod/aws_security_group.web", "tcp 443"},
		{"app/prod/aws_security_group.web", "app/prod/aws_security_group.app", "tcp 8080"},
		{"app/prod/aws_security_group.app", "app/prod/aws_security_group.db", "tcp 5432"},
	} {
		if e := edge(tc.from, tc.to); e.Label != tc.label || e.Action != v1.PlanAction_PLAN_ACTION_CREATE {
			t.Errorf("%s -> %s: %q %v", tc.from, tc.to, e.Label, e.Action)
		}
	}
	if len(topo.Clashes) != 0 {
		t.Errorf("clashes: %v", topo.Clashes)
	}
}

func TestClashKeepsTheFirstWorkspace(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "network", "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	topo, err := Build([]Plan{{"a", "prod", b}, {"b", "prod", b}})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range topo.Nodes {
		if n.Project == "b" && n.AwsId != "" {
			t.Errorf("a clashing node from the second workspace: %s", n.Id)
		}
	}
	if !slices.Contains(topo.Clashes, "vpc-0a1b2c3d: a/prod, b/prod") {
		t.Errorf("clashes: %v", topo.Clashes)
	}
}

func TestSensitiveValuesAreScrubbed(t *testing.T) {
	got := scrub(map[string]any{"name": "web", "tags": map[string]any{"Name": "secret"}, "list": []any{"a", "b"}},
		map[string]any{"tags": map[string]any{"Name": true}, "list": []any{false, true}})
	want := map[string]any{"name": "web", "tags": map[string]any{"Name": "(sensitive)"}, "list": []any{"a", "(sensitive)"}}
	if a, b := jsonOf(got), jsonOf(want); a != b {
		t.Errorf("scrub: %s, want %s", a, b)
	}
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
