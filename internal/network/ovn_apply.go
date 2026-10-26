package network

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strconv"

	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
)

func rowUUID(row model.Model) string {
	return reflect.ValueOf(row).Elem().FieldByName("UUID").String()
}

func rowOwner(row model.Model) map[string]string {
	return reflect.ValueOf(row).Elem().FieldByName("ExternalIDs").Interface().(map[string]string)
}

// Anonymous child rows belong to a stable parent and slot, not to their order
// in the compile result. This also adopts rows written before identity metadata.
func graphKeys(rows []model.Model) map[string]string {
	keys, byID := map[string]string{}, map[string]model.Model{}
	for _, row := range rows {
		id := rowUUID(row)
		byID[id] = row
		if name := reflect.ValueOf(row).Elem().FieldByName("Name"); name.IsValid() {
			keys[id] = reflect.TypeOf(row).String() + "/" + name.String()
		}
	}
	for _, row := range rows {
		switch parent := row.(type) {
		case *SwitchPort:
			for slot, id := range map[string]*string{"v4": parent.DHCPv4, "v6": parent.DHCPv6} {
				if id != nil {
					keys[*id] = "dhcp/" + parent.Name + "/" + slot
				}
			}
		case *Switch:
			for _, id := range parent.ACLs {
				if acl, ok := byID[id].(*ACL); ok {
					keys[id] = "acl/" + parent.Name + "/" + acl.Direction + "/" + strconv.Itoa(acl.Priority)
				}
			}
		case *Router:
			for _, id := range parent.Routes {
				if route, ok := byID[id].(*Route); ok {
					keys[id] = "route/" + parent.Name + "/" + route.Prefix
				}
			}
		}
	}
	return keys
}

func retainGraphUUIDs(current, desired []model.Model) map[string]bool {
	currentKeys, desiredKeys := graphKeys(current), graphKeys(desired)
	existing := map[string]string{}
	for id, key := range currentKeys {
		existing[key] = id
	}
	references, retained := map[string]string{}, map[string]bool{}
	for _, row := range desired {
		id := rowUUID(row)
		if uuid := existing[desiredKeys[id]]; uuid != "" {
			references[id], retained[uuid] = uuid, true
			reflect.ValueOf(row).Elem().FieldByName("UUID").SetString(uuid)
		}
	}
	for _, row := range desired {
		replaceGraphReferences(row, references)
	}
	return retained
}

func replaceGraphReferences(row model.Model, references map[string]string) {
	replace := func(values []string) {
		for i, id := range values {
			if uuid := references[id]; uuid != "" {
				values[i] = uuid
			}
		}
	}
	replaceOne := func(value **string) {
		if *value != nil {
			if uuid := references[**value]; uuid != "" {
				*value = &uuid
			}
		}
	}
	switch row := row.(type) {
	case *Switch:
		replace(row.Ports)
		replace(row.ACLs)
	case *SwitchPort:
		replaceOne(&row.DHCPv4)
		replaceOne(&row.DHCPv6)
	case *Router:
		replace(row.Ports)
		replace(row.Routes)
		replace(row.LoadBalancers)
		replace(row.NAT)
	case *NAT:
		replaceOne(&row.AllowedExtIPs)
	}
}

func changedGraphFields(before, after model.Model) []interface{} {
	a, b := reflect.ValueOf(before).Elem(), reflect.ValueOf(after).Elem()
	var fields []interface{}
	for i := 0; i < b.NumField(); i++ {
		if b.Type().Field(i).Tag.Get("ovsdb") == "_uuid" {
			continue
		}
		left, right := a.Field(i), b.Field(i)
		if (right.Kind() == reflect.Map || right.Kind() == reflect.Slice) && left.Len() == 0 && right.Len() == 0 {
			continue
		}
		if right.Kind() == reflect.Slice {
			x, y := slices.Clone(left.Interface().([]string)), slices.Clone(right.Interface().([]string))
			slices.Sort(x)
			slices.Sort(y)
			if slices.Equal(x, y) {
				continue
			}
		} else if reflect.DeepEqual(left.Interface(), right.Interface()) {
			continue
		}
		fields = append(fields, right.Addr().Interface())
	}
	return fields
}

func (n *OVN) environmentModels(ctx context.Context, environment string, desired []model.Model) ([]model.Model, error) {
	// Get is the library's cache-consistency barrier after reconnect. Conditional
	// cache queries bypass it, so first resolve one real named object from this graph.
	if len(desired) > 0 {
		lookup := model.Clone(desired[0])
		reflect.ValueOf(lookup).Elem().FieldByName("UUID").SetString("")
		if err := n.client.Get(ctx, lookup); err != nil && err != client.ErrNotFound {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	owner, err := ovsdb.NewOvsMap(ownership(environment))
	if err != nil {
		return nil, err
	}
	var rows []model.Model
	cache := n.client.Cache()
	for table := range cache.DatabaseModel().Types() {
		matches, err := cache.Table(table).RowsByCondition([]ovsdb.Condition{{Column: "external_ids", Function: ovsdb.ConditionIncludes, Value: owner}})
		if err != nil {
			return nil, err
		}
		for _, row := range matches {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func (n *OVN) applyModels(ctx context.Context, environment, component string, desired []model.Model) error {
	current, err := n.environmentModels(ctx, environment, desired)
	if err != nil {
		return err
	}
	owned := func(row model.Model) bool {
		return component == "" || rowOwner(row)["netlab.component"] == component
	}
	if component == "access" {
		accessIDs := map[string]bool{}
		for _, row := range current {
			if owned(row) {
				accessIDs[rowUUID(row)] = true
			}
		}
		for _, row := range current {
			if owned(row) {
				continue
			}
			switch parent := model.Clone(row).(type) {
			case *Switch:
				parent.Ports = slices.DeleteFunc(parent.Ports, func(id string) bool { return accessIDs[id] })
				for _, next := range desired {
					if port, ok := next.(*SwitchPort); ok && port.ExternalIDs["netlab.network"] != "" && parent.Name == objectName("ls", environment, port.ExternalIDs["netlab.network"]) {
						parent.Ports = append(parent.Ports, port.UUID)
					}
				}
				desired = append(desired, parent)
			case *Router:
				parent.Ports = slices.DeleteFunc(parent.Ports, func(id string) bool { return accessIDs[id] })
				parent.NAT = slices.DeleteFunc(parent.NAT, func(id string) bool { return accessIDs[id] })
				desired = append(desired, parent)
			}
		}
	}
	retained := retainGraphUUIDs(current, desired)
	byID := map[string]model.Model{}
	for _, row := range current {
		byID[rowUUID(row)] = row
	}
	var operations []ovsdb.Operation
	for _, row := range desired {
		var next []ovsdb.Operation
		if before := byID[rowUUID(row)]; before != nil {
			if fields := changedGraphFields(before, row); len(fields) > 0 {
				next, err = n.client.Where(row).Update(row, fields...)
			}
		} else {
			next, err = n.client.Create(row)
		}
		if err != nil {
			return fmt.Errorf("apply OVN object: %w", err)
		}
		operations = append(operations, next...)
	}
	for _, row := range current {
		if !owned(row) || retained[rowUUID(row)] {
			continue
		}
		next, err := n.client.Where(row).Delete()
		if err != nil {
			return err
		}
		operations = append(operations, next...)
	}
	return transact(ctx, n.client, operations)
}
