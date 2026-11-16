// Package topology joins workspaces' plans into one AWS network: VPCs,
// subnets in zones, gateways, routes and security groups (adr/0038).
package topology

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/internal/graph"
)

// Plan is a workspace's latest plan JSON.
type Plan struct {
	Project, Workspace string
	JSON               []byte
}

// https://opentofu.org/docs/internals/json-format/
type planJSON struct {
	ResourceChanges []struct {
		Address       string `json:"address"`
		ModuleAddress string `json:"module_address"`
		Mode          string `json:"mode"`
		Type          string `json:"type"`
		Name          string `json:"name"`
		IndexKey      any    `json:"index"`
		Change        struct {
			Actions         []string `json:"actions"`
			Before          any      `json:"before"`
			After           any      `json:"after"`
			AfterUnknown    any      `json:"after_unknown"`
			BeforeSensitive any      `json:"before_sensitive"`
			AfterSensitive  any      `json:"after_sensitive"`
		} `json:"change"`
	} `json:"resource_changes"`
	Configuration struct {
		RootModule configModule `json:"root_module"`
	} `json:"configuration"`
}

type configModule struct {
	Resources []struct {
		Address     string                     `json:"address"`
		Mode        string                     `json:"mode"`
		Expressions map[string]json.RawMessage `json:"expressions"`
	} `json:"resources"`
	ModuleCalls map[string]struct {
		Module configModule `json:"module"`
	} `json:"module_calls"`
}

var roles = map[string]v1.TopologyRole{
	"aws_vpc":                             v1.TopologyRole_TOPOLOGY_ROLE_VPC,
	"aws_subnet":                          v1.TopologyRole_TOPOLOGY_ROLE_SUBNET,
	"aws_internet_gateway":                v1.TopologyRole_TOPOLOGY_ROLE_INTERNET_GATEWAY,
	"aws_egress_only_internet_gateway":    v1.TopologyRole_TOPOLOGY_ROLE_INTERNET_GATEWAY,
	"aws_nat_gateway":                     v1.TopologyRole_TOPOLOGY_ROLE_NAT_GATEWAY,
	"aws_route_table":                     v1.TopologyRole_TOPOLOGY_ROLE_ROUTE_TABLE,
	"aws_security_group":                  v1.TopologyRole_TOPOLOGY_ROLE_SECURITY_GROUP,
	"aws_lb":                              v1.TopologyRole_TOPOLOGY_ROLE_LOAD_BALANCER,
	"aws_alb":                             v1.TopologyRole_TOPOLOGY_ROLE_LOAD_BALANCER,
	"aws_elb":                             v1.TopologyRole_TOPOLOGY_ROLE_LOAD_BALANCER,
	"aws_autoscaling_group":               v1.TopologyRole_TOPOLOGY_ROLE_INSTANCE_GROUP,
	"aws_instance":                        v1.TopologyRole_TOPOLOGY_ROLE_INSTANCE,
	"aws_db_instance":                     v1.TopologyRole_TOPOLOGY_ROLE_DATABASE,
	"aws_rds_cluster":                     v1.TopologyRole_TOPOLOGY_ROLE_DATABASE,
	"aws_elasticache_replication_group":   v1.TopologyRole_TOPOLOGY_ROLE_DATABASE,
	"aws_eks_cluster":                     v1.TopologyRole_TOPOLOGY_ROLE_CLUSTER,
	"aws_vpc_peering_connection":          v1.TopologyRole_TOPOLOGY_ROLE_PEERING,
	"aws_route":                           v1.TopologyRole_TOPOLOGY_ROLE_UNSPECIFIED,
	"aws_route_table_association":         v1.TopologyRole_TOPOLOGY_ROLE_UNSPECIFIED,
	"aws_main_route_table_association":    v1.TopologyRole_TOPOLOGY_ROLE_UNSPECIFIED,
	"aws_security_group_rule":             v1.TopologyRole_TOPOLOGY_ROLE_UNSPECIFIED,
	"aws_vpc_security_group_ingress_rule": v1.TopologyRole_TOPOLOGY_ROLE_UNSPECIFIED,
	"aws_vpc_security_group_egress_rule":  v1.TopologyRole_TOPOLOGY_ROLE_UNSPECIFIED,
	"aws_launch_template":                 v1.TopologyRole_TOPOLOGY_ROLE_UNSPECIFIED,
	"aws_db_subnet_group":                 v1.TopologyRole_TOPOLOGY_ROLE_UNSPECIFIED,
	"aws_elasticache_subnet_group":        v1.TopologyRole_TOPOLOGY_ROLE_UNSPECIFIED,
}

// An instance is one resource instance from a plan. Types with no role,
// such as associations and rules, are read for what they say about
// others.
type instance struct {
	plan     int
	module   string
	address  string
	resource string // the address without instance keys, in its module
	key      any
	typ      string
	action   v1.PlanAction
	values   map[string]any
	unknown  map[string]any
	exprs    map[string]json.RawMessage
	node     *v1.TopologyNode
	// dup is an AWS id that an earlier plan has; node is that plan's.
	dup bool
}

type builder struct {
	plans     []Plan
	instances []*instance
	byID      map[string]*instance
	nodes     map[string]*v1.TopologyNode
	edges     map[[3]string]*v1.TopologyEdge
	out       *v1.Topology
}

// Build joins plans, in the order given, into one topology. An AWS id that
// two plans manage is kept from the first and listed as a clash.
func Build(plans []Plan) (*v1.Topology, error) {
	b := &builder{plans: plans, byID: map[string]*instance{}, nodes: map[string]*v1.TopologyNode{},
		edges: map[[3]string]*v1.TopologyEdge{}, out: &v1.Topology{}}
	for i, p := range plans {
		if err := b.read(i, p); err != nil {
			return nil, fmt.Errorf("%s/%s: %w", p.Project, p.Workspace, err)
		}
	}
	b.placeMembers()
	b.routes()
	b.rules()
	for _, in := range b.instances {
		if in.typ == "aws_vpc_peering_connection" && !in.dup {
			b.edge(b.ref(in, "vpc_id"), in.node, v1.TopologyEdgeKind_TOPOLOGY_EDGE_KIND_PEERING, "", in.action)
			b.edge(in.node, b.ref(in, "peer_vpc_id"), v1.TopologyEdgeKind_TOPOLOGY_EDGE_KIND_PEERING, "", in.action)
		}
	}
	for _, n := range b.nodes {
		b.out.Nodes = append(b.out.Nodes, n)
	}
	slices.SortFunc(b.out.Nodes, func(x, y *v1.TopologyNode) int { return strings.Compare(x.Id, y.Id) })
	for _, e := range b.edges {
		b.out.Edges = append(b.out.Edges, e)
	}
	slices.SortFunc(b.out.Edges, func(x, y *v1.TopologyEdge) int {
		return strings.Compare(x.From+" "+x.To+" "+x.Label, y.From+" "+y.To+" "+y.Label)
	})
	return b.out, nil
}

func (b *builder) read(i int, p Plan) error {
	var pj planJSON
	if err := json.Unmarshal(p.JSON, &pj); err != nil {
		return fmt.Errorf("read the plan: %w", err)
	}
	exprs := map[string]map[string]json.RawMessage{}
	var walk func(prefix string, m configModule)
	walk = func(prefix string, m configModule) {
		for _, r := range m.Resources {
			if r.Mode == "managed" {
				exprs[prefix+"\x00"+r.Address] = r.Expressions
			}
		}
		for name, c := range m.ModuleCalls {
			next := "module." + name
			if prefix != "" {
				next = prefix + "." + next
			}
			walk(next, c.Module)
		}
	}
	walk("", pj.Configuration.RootModule)

	for _, rc := range pj.ResourceChanges {
		role, known := roles[rc.Type]
		if rc.Mode != "managed" || !known {
			continue
		}
		values, _ := scrub(rc.Change.After, rc.Change.AfterSensitive).(map[string]any)
		if values == nil {
			values, _ = scrub(rc.Change.Before, rc.Change.BeforeSensitive).(map[string]any)
		}
		unknown, _ := rc.Change.AfterUnknown.(map[string]any)
		module := graph.StripKeys(rc.ModuleAddress)
		resource := rc.Type + "." + rc.Name
		// The topology draws reads and unknown actions as unchanged.
		act := graph.Action(rc.Change.Actions)
		if act == v1.PlanAction_PLAN_ACTION_READ || act == v1.PlanAction_PLAN_ACTION_UNSPECIFIED {
			act = v1.PlanAction_PLAN_ACTION_NO_OP
		}
		in := &instance{plan: i, module: module, address: rc.Address, resource: resource, key: rc.IndexKey, typ: rc.Type,
			action: act, values: values, unknown: unknown, exprs: exprs[module+"\x00"+resource]}
		b.instances = append(b.instances, in)
		id, _ := values["id"].(string)
		if id != "" {
			if first, ok := b.byID[id]; ok {
				b.out.Clashes = append(b.out.Clashes, fmt.Sprintf("%s: %s/%s, %s/%s", id,
					b.plans[first.plan].Project, b.plans[first.plan].Workspace, p.Project, p.Workspace))
				in.node, in.dup = first.node, true
				continue
			}
			b.byID[id] = in
		}
		if role == v1.TopologyRole_TOPOLOGY_ROLE_UNSPECIFIED {
			continue
		}
		n := &v1.TopologyNode{Id: p.Project + "/" + p.Workspace + "/" + rc.Address, Role: role, Project: p.Project,
			Workspace: p.Workspace, Address: rc.Address, Type: rc.Type, AwsId: id, Action: in.action, Name: name(values, rc.Address)}
		switch rc.Type {
		case "aws_vpc":
			n.Cidr = str(values, "cidr_block")
		case "aws_subnet":
			n.Cidr, n.Zone = str(values, "cidr_block"), str(values, "availability_zone")
		}
		in.node = n
		b.nodes[n.Id] = n
	}
	return nil
}

// ref is the node that one of in's attributes names, by AWS id or, while
// the id is unknown, by a reference in the configuration.
func (b *builder) ref(in *instance, attr string) *v1.TopologyNode {
	got := b.refs(in, attr)
	if len(got) == 0 {
		return nil
	}
	return got[0]
}

func (b *builder) refs(in *instance, attr string) []*v1.TopologyNode {
	var out []*v1.TopologyNode
	for _, target := range b.targets(in, in.values[attr], in.unknown[attr], in.exprs[attr]) {
		if target != nil {
			out = append(out, target)
		}
	}
	return out
}

// targets resolves a value that holds ids: a string or a list of them.
// Unknown parts resolve through expr's references.
func (b *builder) targets(in *instance, value, unknown any, expr json.RawMessage) []*v1.TopologyNode {
	var ids []string
	switch v := value.(type) {
	case string:
		ids = append(ids, v)
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok {
				ids = append(ids, s)
			}
		}
	}
	var out []*v1.TopologyNode
	for _, id := range ids {
		if id != "" {
			out = append(out, b.byAWSID(id))
		}
	}
	if isUnknown(unknown) {
		for _, t := range b.referenced(in, references(expr)) {
			out = append(out, t.node)
		}
	}
	return out
}

func (b *builder) byAWSID(id string) *v1.TopologyNode {
	if in, ok := b.byID[id]; ok {
		return in.node
	}
	if n, ok := b.nodes[id]; ok {
		return n
	}
	n := &v1.TopologyNode{Id: id, Role: v1.TopologyRole_TOPOLOGY_ROLE_EXTERNAL, AwsId: id, Name: id,
		Action: v1.PlanAction_PLAN_ACTION_NO_OP}
	b.nodes[id] = n
	return n
}

// referenced finds the instances in in's plan and module that references
// name, such as "aws_subnet.public[0].id" or "aws_vpc.main".
func (b *builder) referenced(in *instance, refs []string) []*instance {
	var out []*instance
	for _, ref := range refs {
		resource, key, keyed := splitRef(ref)
		if resource == "" {
			continue
		}
		var all []*instance
		for _, t := range b.instances {
			if t.plan == in.plan && t.module == in.module && t.resource == resource {
				all = append(all, t)
			}
		}
		switch {
		case keyed:
			for _, t := range all {
				if fmt.Sprint(t.key) == key {
					out = append(out, t)
				}
			}
		case len(all) == 1:
			out = append(out, all[0])
		default:
			// A reference with count.index or each.key: the instance
			// with the referrer's own key, else every instance.
			matched := false
			for _, t := range all {
				if in.key != nil && fmt.Sprint(t.key) == fmt.Sprint(in.key) {
					out, matched = append(out, t), true
				}
			}
			if !matched {
				out = append(out, all...)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return out
}

// splitRef reads "aws_subnet.public[0].id" as resource "aws_subnet.public"
// and key "0". It returns no resource for var., local., data. and module.
// references.
func splitRef(ref string) (resource, key string, keyed bool) {
	parts := strings.SplitN(ref, ".", 3)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "aws_") {
		return "", "", false
	}
	name := parts[1]
	if i := strings.IndexByte(name, '['); i >= 0 {
		key = strings.Trim(name[i+1:], `]"`)
		name, keyed = name[:i], true
	}
	return parts[0] + "." + name, key, keyed
}

// placeMembers sets each node's VPC, subnets, zone and security groups.
func (b *builder) placeMembers() {
	templates := map[string]*instance{}
	groups := map[string]*instance{}
	for _, in := range b.instances {
		switch in.typ {
		case "aws_launch_template":
			if id := str(in.values, "id"); id != "" {
				templates[id] = in
			}
			templates[in.address] = in
		case "aws_db_subnet_group", "aws_elasticache_subnet_group":
			if n := str(in.values, "name"); n != "" {
				groups[fmt.Sprint(in.plan, "\x00", n)] = in
			}
			groups[in.address] = in
		}
	}
	for _, in := range b.instances {
		switch in.typ {
		case "aws_subnet", "aws_internet_gateway", "aws_egress_only_internet_gateway", "aws_route_table", "aws_security_group",
			"aws_vpc_peering_connection":
			if vpc := b.ref(in, "vpc_id"); vpc != nil && !in.dup {
				in.node.Vpc = vpc.Id
			}
		}
	}
	for _, in := range b.instances {
		n := in.node
		if n == nil || in.dup {
			continue
		}
		switch in.typ {
		case "aws_nat_gateway", "aws_instance":
			n.Subnets = ids(b.refs(in, "subnet_id"))
			n.SecurityGroups = ids(b.refs(in, "vpc_security_group_ids"))
		case "aws_lb", "aws_alb", "aws_elb":
			n.Subnets = ids(b.refs(in, "subnets"))
			for i, m := range list(in.values["subnet_mapping"]) {
				mm, _ := m.(map[string]any)
				n.Subnets = append(n.Subnets, ids(b.targets(in, mm["subnet_id"], index(in.unknown["subnet_mapping"], i, "subnet_id"),
					nested(in.exprs["subnet_mapping"], i, "subnet_id")))...)
			}
			n.SecurityGroups = ids(b.refs(in, "security_groups"))
		case "aws_autoscaling_group":
			n.Subnets = ids(b.refs(in, "vpc_zone_identifier"))
			if lt := first(list(in.values["launch_template"])); lt != nil {
				var t *instance
				if id, _ := lt["id"].(string); id != "" {
					t = templates[id]
				}
				if t == nil {
					if ts := b.referenced(in, references(nested(in.exprs["launch_template"], 0, "id"))); len(ts) > 0 {
						t = ts[0]
					}
				}
				if t != nil {
					n.SecurityGroups = ids(b.refs(t, "vpc_security_group_ids"))
				}
			}
		case "aws_db_instance", "aws_rds_cluster", "aws_elasticache_replication_group":
			attr := "db_subnet_group_name"
			if in.typ == "aws_elasticache_replication_group" {
				attr = "subnet_group_name"
			}
			g := groups[fmt.Sprint(in.plan, "\x00", str(in.values, attr))]
			if g == nil {
				if gs := b.referenced(in, references(in.exprs[attr])); len(gs) > 0 {
					g = gs[0]
				}
			}
			if g != nil {
				n.Subnets = ids(b.refs(g, "subnet_ids"))
			}
			n.SecurityGroups = ids(b.refs(in, "vpc_security_group_ids"))
			if len(n.SecurityGroups) == 0 {
				n.SecurityGroups = ids(b.refs(in, "security_group_ids"))
			}
		case "aws_eks_cluster":
			if vc := first(list(in.values["vpc_config"])); vc != nil {
				n.Subnets = ids(b.targets(in, vc["subnet_ids"], index(in.unknown["vpc_config"], 0, "subnet_ids"),
					nested(in.exprs["vpc_config"], 0, "subnet_ids")))
				n.SecurityGroups = ids(b.targets(in, vc["security_group_ids"], index(in.unknown["vpc_config"], 0, "security_group_ids"),
					nested(in.exprs["vpc_config"], 0, "security_group_ids")))
			}
		}
		zones := map[string]bool{}
		for _, s := range n.Subnets {
			if sn := b.nodes[s]; sn != nil {
				if n.Vpc == "" {
					n.Vpc = sn.Vpc
				}
				zones[sn.Zone] = true
			}
		}
		if len(zones) == 1 && n.Zone == "" {
			for z := range zones {
				n.Zone = z
			}
		}
	}
}

// routes gives each subnet its route table's routes, and an edge to each
// route's target. A subnet with no association uses its VPC's main table.
func (b *builder) routes() {
	tables := map[*v1.TopologyNode][]*v1.TopologyRoute{}
	actions := map[*v1.TopologyNode]v1.PlanAction{}
	targetAttrs := []string{"gateway_id", "nat_gateway_id", "transit_gateway_id", "vpc_peering_connection_id",
		"egress_only_gateway_id", "network_interface_id", "vpc_endpoint_id", "local_gateway_id", "carrier_gateway_id"}
	route := func(in *instance, values, unknown map[string]any, exprs func(string) json.RawMessage) *v1.TopologyRoute {
		r := &v1.TopologyRoute{Destination: firstOf(values, "cidr_block", "ipv6_cidr_block", "destination_prefix_list_id",
			"destination_cidr_block", "destination_ipv6_cidr_block")}
		for _, attr := range targetAttrs {
			if t := b.targets(in, values[attr], unknown[attr], exprs(attr)); len(t) > 0 && t[0] != nil {
				r.Target = t[0].Id
				return r
			}
		}
		return nil
	}
	for _, in := range b.instances {
		if in.dup {
			continue
		}
		switch in.typ {
		case "aws_route_table":
			for i, v := range list(in.values["route"]) {
				values, _ := v.(map[string]any)
				unknown, _ := index(in.unknown["route"], i, "").(map[string]any)
				if r := route(in, values, unknown, func(attr string) json.RawMessage { return nested(in.exprs["route"], i, attr) }); r != nil {
					tables[in.node] = append(tables[in.node], r)
				}
			}
			actions[in.node] = in.action
		case "aws_route":
			table := b.ref(in, "route_table_id")
			if table == nil {
				continue
			}
			if r := route(in, in.values, in.unknown, func(attr string) json.RawMessage { return in.exprs[attr] }); r != nil {
				tables[table] = append(tables[table], r)
			}
		}
	}
	associated := map[string]*v1.TopologyNode{}
	mains := map[string]*v1.TopologyNode{}
	for _, in := range b.instances {
		if in.dup {
			continue
		}
		switch in.typ {
		case "aws_route_table_association":
			if s, t := b.ref(in, "subnet_id"), b.ref(in, "route_table_id"); s != nil && t != nil {
				associated[s.Id] = t
			}
		case "aws_main_route_table_association":
			if v, t := b.ref(in, "vpc_id"), b.ref(in, "route_table_id"); v != nil && t != nil {
				mains[v.Id] = t
			}
		case "aws_vpc":
			if id := str(in.values, "main_route_table_id"); id != "" && mains[in.node.Id] == nil {
				if t, ok := b.byID[id]; ok && t.node != nil {
					mains[in.node.Id] = t.node
				}
			}
		}
	}
	for _, s := range b.nodes {
		if s.Role != v1.TopologyRole_TOPOLOGY_ROLE_SUBNET {
			continue
		}
		table := associated[s.Id]
		if table == nil {
			table = mains[s.Vpc]
		}
		if table == nil {
			continue
		}
		s.Routes = tables[table]
		for _, r := range s.Routes {
			target := b.nodes[r.Target]
			if target.GetRole() == v1.TopologyRole_TOPOLOGY_ROLE_INTERNET_GATEWAY && (r.Destination == "0.0.0.0/0" || r.Destination == "::/0") {
				s.Public = true
			}
			a := actions[table]
			if target.GetAction() == v1.PlanAction_PLAN_ACTION_CREATE {
				a = v1.PlanAction_PLAN_ACTION_CREATE
			}
			b.edge(s, target, v1.TopologyEdgeKind_TOPOLOGY_EDGE_KIND_ROUTE, r.Destination, a)
		}
	}
}

// rules gives each security group its rules, from the group's own blocks
// and from rule resources, and an edge from each source.
func (b *builder) rules() {
	internet := func() *v1.TopologyNode {
		if n, ok := b.nodes["internet"]; ok {
			return n
		}
		n := &v1.TopologyNode{Id: "internet", Role: v1.TopologyRole_TOPOLOGY_ROLE_INTERNET, Name: "Internet",
			Action: v1.PlanAction_PLAN_ACTION_NO_OP}
		b.nodes[n.Id] = n
		return n
	}
	add := func(group *v1.TopologyNode, r *v1.TopologyRule, action v1.PlanAction) {
		group.Rules = append(group.Rules, r)
		if !r.Ingress {
			return
		}
		label := strings.TrimSpace(ports(r))
		if r.SecurityGroup != "" {
			b.edge(b.nodes[r.SecurityGroup], group, v1.TopologyEdgeKind_TOPOLOGY_EDGE_KIND_SECURITY_GROUP_RULE, label, action)
		}
		for _, c := range r.Cidrs {
			if c == "0.0.0.0/0" || c == "::/0" {
				b.edge(internet(), group, v1.TopologyEdgeKind_TOPOLOGY_EDGE_KIND_SECURITY_GROUP_RULE, label, action)
				break
			}
		}
	}
	for _, in := range b.instances {
		if in.dup {
			continue
		}
		switch in.typ {
		case "aws_security_group":
			for _, dir := range []string{"ingress", "egress"} {
				for i, v := range list(in.values[dir]) {
					values, _ := v.(map[string]any)
					unknown, _ := index(in.unknown[dir], i, "").(map[string]any)
					r := rule(values, dir == "ingress")
					r.Cidrs = append(strs(values["cidr_blocks"]), strs(values["ipv6_cidr_blocks"])...)
					if g := b.targets(in, values["security_groups"], unknown["security_groups"], nested(in.exprs[dir], i, "security_groups")); len(g) > 0 && g[0] != nil {
						r.SecurityGroup = g[0].Id
					}
					add(in.node, r, in.action)
				}
			}
		case "aws_security_group_rule":
			group := b.ref(in, "security_group_id")
			if group == nil {
				continue
			}
			r := rule(in.values, str(in.values, "type") == "ingress")
			r.Cidrs = append(strs(in.values["cidr_blocks"]), strs(in.values["ipv6_cidr_blocks"])...)
			if g := b.ref(in, "source_security_group_id"); g != nil {
				r.SecurityGroup = g.Id
			}
			add(group, r, in.action)
		case "aws_vpc_security_group_ingress_rule", "aws_vpc_security_group_egress_rule":
			group := b.ref(in, "security_group_id")
			if group == nil {
				continue
			}
			r := &v1.TopologyRule{Ingress: in.typ == "aws_vpc_security_group_ingress_rule", Protocol: str(in.values, "ip_protocol"),
				FromPort: num(in.values, "from_port"), ToPort: num(in.values, "to_port"), Description: str(in.values, "description")}
			for _, c := range []string{str(in.values, "cidr_ipv4"), str(in.values, "cidr_ipv6")} {
				if c != "" {
					r.Cidrs = append(r.Cidrs, c)
				}
			}
			if g := b.ref(in, "referenced_security_group_id"); g != nil {
				r.SecurityGroup = g.Id
			}
			add(group, r, in.action)
		}
	}
}

func rule(values map[string]any, ingress bool) *v1.TopologyRule {
	self, _ := values["self"].(bool)
	return &v1.TopologyRule{Ingress: ingress, Protocol: str(values, "protocol"), FromPort: num(values, "from_port"),
		ToPort: num(values, "to_port"), Self: self, Description: str(values, "description")}
}

func ports(r *v1.TopologyRule) string {
	switch {
	case r.Protocol == "-1" || r.Protocol == "all":
		return "all traffic"
	case r.FromPort == r.ToPort:
		return fmt.Sprintf("%s %d", r.Protocol, r.FromPort)
	}
	return fmt.Sprintf("%s %d-%d", r.Protocol, r.FromPort, r.ToPort)
}

func (b *builder) edge(from, to *v1.TopologyNode, kind v1.TopologyEdgeKind, label string, a v1.PlanAction) {
	if from == nil || to == nil {
		return
	}
	k := [3]string{from.Id, to.Id, label}
	if e, ok := b.edges[k]; ok {
		e.Action = max(e.Action, a)
		return
	}
	b.edges[k] = &v1.TopologyEdge{From: from.Id, To: to.Id, Kind: kind, Label: label, Action: a}
}

func name(values map[string]any, address string) string {
	if tags, ok := values["tags"].(map[string]any); ok {
		if n, ok := tags["Name"].(string); ok && n != "" {
			return n
		}
	}
	for _, attr := range []string{"name", "identifier"} {
		if n := str(values, attr); n != "" {
			return n
		}
	}
	return address
}

// scrub replaces the values that marks flags sensitive.
// https://opentofu.org/docs/internals/json-format/#change-representation
func scrub(v, marks any) any {
	switch m := marks.(type) {
	case bool:
		if m {
			return "(sensitive)"
		}
	case map[string]any:
		if vm, ok := v.(map[string]any); ok {
			for k, mk := range m {
				if _, ok := vm[k]; ok {
					vm[k] = scrub(vm[k], mk)
				}
			}
		}
	case []any:
		if vl, ok := v.([]any); ok {
			for i := range min(len(vl), len(m)) {
				vl[i] = scrub(vl[i], m[i])
			}
		}
	}
	return v
}

// references reads an expression's {"references": [...]}.
func references(expr json.RawMessage) []string {
	var e struct {
		References []string `json:"references"`
	}
	_ = json.Unmarshal(expr, &e)
	return e.References
}

// nested is the expression of attr in block i of a block's expressions,
// which the JSON gives as a list of objects, or as one expression for the
// whole of an attribute written as blocks.
func nested(expr json.RawMessage, i int, attr string) json.RawMessage {
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(expr, &blocks) == nil {
		if i < len(blocks) {
			return blocks[i][attr]
		}
		return nil
	}
	return expr
}

// index reads after_unknown for block i: true when all of it is unknown.
func index(unknown any, i int, attr string) any {
	if unknown == true {
		return true
	}
	l, _ := unknown.([]any)
	if i >= len(l) {
		return nil
	}
	if attr == "" {
		return l[i]
	}
	m, _ := l[i].(map[string]any)
	return m[attr]
}

func isUnknown(u any) bool {
	switch v := u.(type) {
	case bool:
		return v
	case []any:
		return slices.ContainsFunc(v, isUnknown)
	}
	return false
}

func ids(ns []*v1.TopologyNode) []string {
	var out []string
	for _, n := range ns {
		if n != nil && !slices.Contains(out, n.Id) {
			out = append(out, n.Id)
		}
	}
	return out
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func first(l []any) map[string]any {
	if len(l) == 0 {
		return nil
	}
	m, _ := l[0].(map[string]any)
	return m
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func firstOf(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := str(m, k); s != "" {
			return s
		}
	}
	return ""
}

func strs(v any) []string {
	var out []string
	for _, x := range list(v) {
		if s, ok := x.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func num(m map[string]any, k string) int32 {
	f, _ := m[k].(float64)
	return int32(f)
}

// Scope keeps a project's own nodes, and as context what they use from
// other projects: their VPC, subnets, security groups and route targets,
// and each context subnet's VPC and route targets. An edge stays when one
// end is the project's own (adr/0039).
func Scope(t *v1.Topology, project string) *v1.Topology {
	byID := map[string]*v1.TopologyNode{}
	for _, n := range t.Nodes {
		byID[n.Id] = n
	}
	own := map[string]bool{}
	for _, n := range t.Nodes {
		if n.Project == project {
			own[n.Id] = true
		}
	}
	context := map[string]bool{}
	add := func(id string) {
		if id != "" && !own[id] && byID[id] != nil {
			context[id] = true
		}
	}
	for id := range own {
		n := byID[id]
		add(n.Vpc)
		for _, s := range n.Subnets {
			add(s)
		}
		for _, g := range n.SecurityGroups {
			add(g)
		}
		for _, r := range n.Routes {
			add(r.Target)
		}
	}
	var edges []*v1.TopologyEdge
	for _, e := range t.Edges {
		if own[e.From] || own[e.To] {
			edges = append(edges, e)
			add(e.From)
			add(e.To)
		}
	}
	var subnets []*v1.TopologyNode
	for id := range context {
		if byID[id].Role == v1.TopologyRole_TOPOLOGY_ROLE_SUBNET {
			subnets = append(subnets, byID[id])
		}
	}
	for _, s := range subnets {
		add(s.Vpc)
		for _, r := range s.Routes {
			add(r.Target)
		}
	}
	out := &v1.Topology{Edges: edges}
	for _, n := range t.Nodes {
		switch {
		case own[n.Id]:
			out.Nodes = append(out.Nodes, n)
		case context[n.Id]:
			c := proto.Clone(n).(*v1.TopologyNode)
			c.Context = c.Project != ""
			c.Findings, c.Worst = 0, v1.Severity_SEVERITY_UNSPECIFIED
			out.Nodes = append(out.Nodes, c)
		}
	}
	for _, c := range t.Clashes {
		if strings.Contains(c, " "+project+"/") {
			out.Clashes = append(out.Clashes, c)
		}
	}
	return out
}
