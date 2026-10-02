package network

import (
	"context"
	"fmt"

	"github.com/ovn-org/libovsdb/ovsdb"
)

func (n *OVN) applyVPN(ctx context.Context, environment string, record *vpnRecord) error {
	var routers []Router
	if err := n.client.Where(&Router{Name: objectName("lr", environment, "gateway")}).List(ctx, &routers); err != nil {
		return err
	}
	if len(routers) != 1 {
		return fmt.Errorf("VPN environment router: expected one match, found %d", len(routers))
	}
	router := &routers[0]
	owned := func(ids map[string]string) bool {
		return ids["netlab.environment"] == environment && ids["netlab.component"] == "vpn"
	}
	var ports []RouterPort
	if err := n.client.WhereCache(func(port *RouterPort) bool { return owned(port.ExternalIDs) }).List(ctx, &ports); err != nil {
		return err
	}
	var nats []NAT
	if err := n.client.WhereCache(func(nat *NAT) bool { return owned(nat.ExternalIDs) }).List(ctx, &nats); err != nil {
		return err
	}
	removeVPNReferences(router, ports, nats)
	ids := ownership(environment)
	ids["netlab.component"] = "vpn"
	value, err := ovsdb.NewOvsMap(ids)
	if err != nil {
		return err
	}
	var operations []ovsdb.Operation
	for _, table := range []string{"Logical_Switch", "Logical_Switch_Port", "Logical_Router_Port", "NAT", "Address_Set"} {
		operations = append(operations, ovsdb.Operation{Op: ovsdb.OperationDelete, Table: table, Where: []ovsdb.Condition{{Column: "external_ids", Function: ovsdb.ConditionIncludes, Value: value}}})
	}
	if models := vpnModels(environment, record, router, n.chassis); len(models) > 0 {
		create, err := n.client.Create(models...)
		if err != nil {
			return err
		}
		operations = append(operations, create...)
	}
	update, err := n.client.Where(router).Update(router, &router.Ports, &router.NAT, &router.Options)
	if err != nil {
		return err
	}
	return transact(ctx, n.client, append(operations, update...))
}
