package network

import (
	"context"
	"github.com/ovn-org/libovsdb/ovsdb"
	"netlab.local/core/api"
	"slices"
)

func (n *OVN) applyAccess(ctx context.Context, environment string, record *accessRecord, gateway *api.ServiceGateway) error {
	models, err := accessModels(environment, record, gateway, n.provider)
	if err != nil {
		return err
	}
	owned := func(ids map[string]string) bool {
		return ids["netlab.environment"] == environment && ids["netlab.component"] == "access"
	}
	var ports []SwitchPort
	if err = n.client.WhereCache(func(p *SwitchPort) bool { return owned(p.ExternalIDs) }).List(ctx, &ports); err != nil {
		return err
	}
	old := map[string]bool{}
	for _, port := range ports {
		old[port.UUID] = true
	}
	var switches []Switch
	if err = n.client.WhereCache(func(sw *Switch) bool {
		return sw.ExternalIDs["netlab.environment"] == environment && !owned(sw.ExternalIDs)
	}).List(ctx, &switches); err != nil {
		return err
	}
	value, err := ovsdb.NewOvsMap(map[string]string{"netlab.environment": environment, "netlab.component": "access"})
	if err != nil {
		return err
	}
	var ops []ovsdb.Operation
	for _, table := range []string{"Logical_Switch", "Logical_Switch_Port", "Logical_Router_Port", "NAT", "Address_Set"} {
		ops = append(ops, ovsdb.Operation{Op: ovsdb.OperationDelete, Table: table, Where: []ovsdb.Condition{{Column: "external_ids", Function: ovsdb.ConditionIncludes, Value: value}}})
	}
	if len(models) > 0 {
		create, err := n.client.Create(models...)
		if err != nil {
			return err
		}
		ops = append(ops, create...)
	}
	for i := range switches {
		sw := &switches[i]
		sw.Ports = slices.DeleteFunc(sw.Ports, func(id string) bool { return old[id] })
		for _, item := range models {
			if port, ok := item.(*SwitchPort); ok && port.ExternalIDs["netlab.network"] != "" && sw.Name == objectName("ls", environment, port.ExternalIDs["netlab.network"]) {
				sw.Ports = append(sw.Ports, port.UUID)
			}
		}
		update, err := n.client.Where(sw).Update(sw, &sw.Ports)
		if err != nil {
			return err
		}
		ops = append(ops, update...)
	}
	return transact(ctx, n.client, ops)
}
