package network

import (
	"net/netip"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/ovn-kubernetes/libovsdb/model"
	"netlab.local/core/api"
)

func stableGraphPlan() api.NodePlan {
	left, right, router := "10.0.0.1", "192.0.2.1", "router"
	management := "10.0.0.254"
	source, destination := "10.0.0.0/24", "192.0.2.0/24"
	routers := []api.LogicalRouter{{Id: router, Name: "Router", Interfaces: []api.RouterInterface{
		{Id: "r-left", NetworkId: "left", Address: left}, {Id: "r-right", NetworkId: "right", Address: right},
	}}}
	routes := []api.Route{{RouterId: &router, NetworkId: "left", Destination: "198.51.100.0/24", NextHop: "10.0.0.253"}}
	policies := []api.Policy{{Id: "policy", NetworkId: "left", Action: api.Allow, Direction: api.Both, Source: &source, Destination: &destination}}
	plan := api.NodePlan{EnvironmentId: "stable", Spec: api.EnvironmentSpec{
		Networks: []api.Network{{Id: "left", Name: "Left", Cidr: source, Gateway: &left, AccessAddress: &management}, {Id: "right", Name: "Right", Cidr: destination, Gateway: &right}},
		Routers:  &routers, Routes: &routes, Policies: &policies,
	}}
	for index, network := range plan.Spec.Networks {
		address := netip.MustParsePrefix(network.Cidr).Addr().Next().Next().String()
		iface := api.Interface{Id: network.Id, NetworkId: network.Id, Address: address, Mac: []string{"02:00:00:00:00:01", "02:00:00:00:00:02"}[index], Primary: true}
		asset := api.Asset{Id: network.Id, Interfaces: []api.Interface{iface}}
		plan.Spec.Assets = append(plan.Spec.Assets, asset)
		plan.Assets = append(plan.Assets, api.AssetExecution{Asset: asset, Interfaces: []api.ResolvedInterface{{Id: iface.Id, PortName: "port-" + iface.Id}}})
	}
	return plan
}

func databaseGraph(rows []model.Model) []model.Model {
	references := map[string]string{}
	for _, row := range rows {
		references[rowUUID(row)] = uuid.NewString()
	}
	result := make([]model.Model, len(rows))
	for i, row := range rows {
		result[i] = model.Clone(row)
		reflect.ValueOf(result[i]).Elem().FieldByName("UUID").SetString(references[rowUUID(row)])
		replaceGraphReferences(result[i], references)
	}
	return result
}

func TestGraphApplyRetainsDatabaseUUIDsAndReferences(t *testing.T) {
	plan := stableGraphPlan()
	compiled, err := Compile(plan)
	if err != nil {
		t.Fatal(err)
	}
	current := databaseGraph(compiled)
	before := graphKeys(current)
	slices.Reverse(plan.Spec.Networks)
	slices.Reverse(plan.Spec.Assets)
	desired, err := Compile(plan)
	if err != nil {
		t.Fatal(err)
	}
	retained := retainGraphUUIDs(current, desired)
	if len(retained) != len(current) {
		t.Fatalf("stable graph replaced database rows: retained=%d current=%d", len(retained), len(current))
	}
	byID := map[string]model.Model{}
	for _, row := range current {
		byID[rowUUID(row)] = row
	}
	for _, row := range desired {
		id := rowUUID(row)
		if before[id] == "" || len(changedGraphFields(byID[id], row)) != 0 {
			t.Fatalf("reordering caused an unnecessary object update: %+v", row)
		}
	}
	if !reflect.DeepEqual(before, graphKeys(desired)) {
		t.Fatal("named UUID references were not replaced by retained database UUIDs")
	}
}

func TestGraphUpdatesIncludeClearedAndSetValuedFields(t *testing.T) {
	tag, dhcp := 100, "existing-dhcp"
	before := &SwitchPort{UUID: "port", Name: "port", TagRequest: &tag, DHCPv4: &dhcp, Options: map[string]string{"router-port": "old"}, Addresses: []string{"b", "a"}}
	after := &SwitchPort{UUID: "port", Name: "port", Addresses: []string{"a", "b"}}
	fields := changedGraphFields(before, after)
	if len(fields) != 3 {
		t.Fatalf("clearing optional/map fields must be explicit, set order must not update: %d", len(fields))
	}
}

func TestGraphApplyUpdatesDHCPWithoutReplacingItsPort(t *testing.T) {
	plan := stableGraphPlan()
	compiled, err := Compile(plan)
	if err != nil {
		t.Fatal(err)
	}
	current := databaseGraph(compiled)
	mtu := 1300
	plan.Spec.Networks[0].Mtu = &mtu
	desired, err := Compile(plan)
	if err != nil {
		t.Fatal(err)
	}
	retainGraphUUIDs(current, desired)
	byID := map[string]model.Model{}
	for _, row := range current {
		byID[rowUUID(row)] = row
	}
	changed := 0
	for _, row := range desired {
		if fields := changedGraphFields(byID[rowUUID(row)], row); len(fields) > 0 {
			changed++
			if _, ok := row.(*DHCP); !ok || len(fields) != 1 {
				t.Fatalf("MTU edit changed a datapath or unrelated field: %+v", row)
			}
		}
	}
	if changed != 1 {
		t.Fatalf("MTU change touched %d objects, want its one DHCP row", changed)
	}
}
