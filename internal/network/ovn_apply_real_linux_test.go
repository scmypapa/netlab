//go:build linux

package network

import (
	"context"
	"net/netip"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	"netlab.local/core/api"
)

type countedOVNClient struct {
	client.Client
	writes atomic.Int64
}

func (c *countedOVNClient) Transact(ctx context.Context, operations ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	c.writes.Add(1)
	return c.Client.Transact(ctx, operations...)
}

func TestRealOVNApplyPreservesDatabaseIdentity(t *testing.T) {
	if os.Getenv("NETLAB_REAL_NETWORK") != "1" {
		t.Skip("set NETLAB_REAL_NETWORK=1 on a configured OVN worker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	n, err := NewOVN(ctx, "unix:/run/ovn/ovnnb_db.sock", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	counted := &countedOVNClient{Client: n.client}
	n.client = counted
	plan := stableGraphPlan()
	plan.EnvironmentId = uuid.NewString()
	for i := range plan.Assets {
		plan.Assets[i].Interfaces[0].PortName = objectName("test_identity", plan.EnvironmentId, plan.Assets[i].Asset.Id)
	}
	leftPort := plan.Assets[0].Interfaces[0].PortName
	defer func() {
		if err := n.Remove(context.Background(), plan.EnvironmentId); err != nil {
			t.Error(err)
		}
	}()
	n.provider = netip.MustParsePrefix("100.127.0.0/16")
	plan.Gateway = &api.ServiceGateway{NodeId: "test", Address: "100.127.10.2"}
	record := &accessRecord{Networks: plan.Spec.Networks}
	n.accessRecord = func(string) *accessRecord { return record }
	if err := n.Apply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	identity := func() map[string]string {
		t.Helper()
		rows, err := n.environmentModels(ctx, plan.EnvironmentId, []model.Model{&Switch{Name: objectName("ls", plan.EnvironmentId, "left")}})
		if err != nil {
			t.Fatal(err)
		}
		result := map[string]string{}
		for id, key := range graphKeys(rows) {
			result[key] = id
		}
		return result
	}
	before := identity()
	writes := counted.writes.Load()
	for range 5 {
		if err := n.Apply(ctx, plan); err != nil {
			t.Fatal(err)
		}
		if err := n.applyAccess(ctx, plan.EnvironmentId, record, plan.Gateway); err != nil {
			t.Fatal(err)
		}
		if after := identity(); !reflect.DeepEqual(before, after) {
			t.Fatalf("unchanged business/access application replaced database identities: before=%v after=%v", before, after)
		}
	}
	if actual := counted.writes.Load(); actual != writes {
		t.Fatalf("unchanged graph wrote unnecessary transactions: before=%d after=%d", writes, actual)
	}
	mtu := 1350
	plan.Spec.Networks[0].Mtu = &mtu
	if err := n.Apply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if after := identity(); !reflect.DeepEqual(before, after) {
		t.Fatal("updating DHCP replaced topology, policy, or management identities")
	}
	plan.Spec.Assets = plan.Spec.Assets[1:]
	plan.Assets = plan.Assets[1:]
	if err := n.Apply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	after := identity()
	if after["*network.SwitchPort/"+leftPort] != "" || after["dhcp/"+leftPort+"/v4"] != "" {
		t.Fatal("removed asset retained its logical port or DHCP row")
	}
	for key, id := range after {
		if before[key] != id {
			t.Fatalf("removing one asset replaced another object %s", key)
		}
	}
	if err := n.applyAccess(ctx, plan.EnvironmentId, nil, nil); err != nil {
		t.Fatal(err)
	}
	rows, err := n.environmentModels(ctx, plan.EnvironmentId, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if rowOwner(row)["netlab.component"] == "access" {
			t.Fatalf("revoked access retained an object: %+v", row)
		}
	}
	if got := identity()["*network.Switch/"+objectName("ls", plan.EnvironmentId, "left")]; got != before["*network.Switch/"+objectName("ls", plan.EnvironmentId, "left")] {
		t.Fatal("removing management access replaced its business switch")
	}
}
