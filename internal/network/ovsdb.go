package network

import (
	"context"
	"fmt"
	"github.com/go-logr/logr"
	"log/slog"
	"net/netip"
	"reflect"
	"strings"

	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
)

type Switch struct {
	UUID        string            `ovsdb:"_uuid"`
	Name        string            `ovsdb:"name"`
	Ports       []string          `ovsdb:"ports"`
	ACLs        []string          `ovsdb:"acls"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}
type SwitchPort struct {
	UUID         string            `ovsdb:"_uuid"`
	Name         string            `ovsdb:"name"`
	Type         string            `ovsdb:"type"`
	Addresses    []string          `ovsdb:"addresses"`
	PortSecurity []string          `ovsdb:"port_security"`
	Options      map[string]string `ovsdb:"options"`
	DHCPv4       *string           `ovsdb:"dhcpv4_options"`
	DHCPv6       *string           `ovsdb:"dhcpv6_options"`
	ExternalIDs  map[string]string `ovsdb:"external_ids"`
}
type DHCP struct {
	UUID        string            `ovsdb:"_uuid"`
	CIDR        string            `ovsdb:"cidr"`
	Options     map[string]string `ovsdb:"options"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}
type Router struct {
	UUID          string            `ovsdb:"_uuid"`
	Name          string            `ovsdb:"name"`
	Ports         []string          `ovsdb:"ports"`
	Routes        []string          `ovsdb:"static_routes"`
	LoadBalancers []string          `ovsdb:"load_balancer"`
	NAT           []string          `ovsdb:"nat"`
	Options       map[string]string `ovsdb:"options"`
	ExternalIDs   map[string]string `ovsdb:"external_ids"`
}
type AddressSet struct {
	UUID        string            `ovsdb:"_uuid"`
	Name        string            `ovsdb:"name"`
	Addresses   []string          `ovsdb:"addresses"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}
type NAT struct {
	UUID          string            `ovsdb:"_uuid"`
	Type          string            `ovsdb:"type"`
	LogicalIP     string            `ovsdb:"logical_ip"`
	ExternalIP    string            `ovsdb:"external_ip"`
	AllowedExtIPs *string           `ovsdb:"allowed_ext_ips"`
	ExternalIDs   map[string]string `ovsdb:"external_ids"`
}
type RouterPort struct {
	UUID        string            `ovsdb:"_uuid"`
	Name        string            `ovsdb:"name"`
	MAC         string            `ovsdb:"mac"`
	Networks    []string          `ovsdb:"networks"`
	IPv6RA      map[string]string `ovsdb:"ipv6_ra_configs"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}
type Route struct {
	UUID        string            `ovsdb:"_uuid"`
	Prefix      string            `ovsdb:"ip_prefix"`
	NextHop     string            `ovsdb:"nexthop"`
	OutputPort  *string           `ovsdb:"output_port"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}
type ACL struct {
	UUID        string            `ovsdb:"_uuid"`
	Direction   string            `ovsdb:"direction"`
	Priority    int               `ovsdb:"priority"`
	Match       string            `ovsdb:"match"`
	Action      string            `ovsdb:"action"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}
type Bridge struct {
	UUID        string            `ovsdb:"_uuid"`
	Name        string            `ovsdb:"name"`
	Ports       []string          `ovsdb:"ports"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}
type OpenVSwitch struct {
	UUID        string            `ovsdb:"_uuid"`
	Bridges     []string          `ovsdb:"bridges"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}
type LoadBalancer struct {
	UUID        string            `ovsdb:"_uuid"`
	Name        string            `ovsdb:"name"`
	Protocol    *string           `ovsdb:"protocol"`
	VIPs        map[string]string `ovsdb:"vips"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}
type Port struct {
	UUID        string            `ovsdb:"_uuid"`
	Name        string            `ovsdb:"name"`
	Interfaces  []string          `ovsdb:"interfaces"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}
type Interface struct {
	UUID        string            `ovsdb:"_uuid"`
	Name        string            `ovsdb:"name"`
	Type        string            `ovsdb:"type"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}

type OVN struct {
	client    client.Client
	provider  netip.Prefix
	chassis   string
	vpnRecord func(string) *vpnRecord
}
type OVS struct {
	client client.Client
	bridge string
}

func NewOVN(ctx context.Context, endpoint string) (*OVN, error) {
	tables := map[string]model.Model{
		"Logical_Switch": &Switch{}, "Logical_Switch_Port": &SwitchPort{}, "DHCP_Options": &DHCP{},
		"Logical_Router": &Router{}, "Logical_Router_Port": &RouterPort{}, "Logical_Router_Static_Route": &Route{}, "ACL": &ACL{},
		"Load_Balancer": &LoadBalancer{}, "NAT": &NAT{}, "Address_Set": &AddressSet{},
	}
	db, err := model.NewClientDBModel("OVN_Northbound", tables)
	if err != nil {
		return nil, err
	}
	// OVN routers have no schema name index; Netlab assigns stable router names.
	db.SetIndexes(map[string][]model.ClientIndex{
		"Logical_Router": {{Columns: []model.ColumnKey{{Column: "name"}}}},
	})
	c, err := connect(ctx, endpoint, db, tables)
	if err != nil {
		return nil, err
	}
	return &OVN{client: c}, nil
}
func NewOVS(ctx context.Context, endpoint, bridge string) (*OVS, error) {
	tables := map[string]model.Model{"Open_vSwitch": &OpenVSwitch{}, "Bridge": &Bridge{}, "Port": &Port{}, "Interface": &Interface{}}
	db, err := model.NewClientDBModel("Open_vSwitch", tables)
	if err != nil {
		return nil, err
	}
	c, err := connect(ctx, endpoint, db, tables)
	if err != nil {
		return nil, err
	}
	b := Bridge{Name: bridge}
	if err = c.Get(ctx, &b); err != nil {
		c.Close()
		return nil, fmt.Errorf("OVS integration bridge %s: %w", bridge, err)
	}
	return &OVS{client: c, bridge: bridge}, nil
}
func connect(ctx context.Context, endpoint string, db model.ClientDBModel, tables map[string]model.Model) (client.Client, error) {
	logger := logr.FromSlogHandler(slog.Default().Handler())
	c, err := client.NewOVSDBClient(db, client.WithEndpoint(endpoint), client.WithLogger(&logger))
	if err != nil {
		return nil, err
	}
	if err = c.Connect(ctx); err != nil {
		return nil, err
	}
	options := []client.MonitorOption{}
	for _, m := range tables {
		fields := []interface{}{}
		value := reflect.ValueOf(m).Elem()
		for i := 0; i < value.NumField(); i++ {
			if value.Type().Field(i).Tag.Get("ovsdb") != "_uuid" {
				fields = append(fields, value.Field(i).Addr().Interface())
			}
		}
		options = append(options, client.WithTable(m, fields...))
	}
	if _, err = c.Monitor(ctx, c.NewMonitor(options...)); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}
func transact(ctx context.Context, c client.Client, ops []ovsdb.Operation) error {
	if len(ops) == 0 {
		return nil
	}
	r, err := c.Transact(ctx, ops...)
	if err != nil {
		return err
	}
	if errors, err := ovsdb.CheckOperationResults(r, ops); err != nil {
		return fmt.Errorf("OVSDB transaction: %w (%v)", err, errors)
	}
	return nil
}
func (n *OVN) Close() { n.client.Close() }
func (n *OVS) Close() { n.client.Close() }

func (n *OVS) Attach(ctx context.Context, device, logicalPort, environmentID, assetID, instanceID string) error {
	p := Port{Name: device}
	if err := n.client.Get(ctx, &p); err == nil {
		if p.ExternalIDs["netlab.instance"] != instanceID {
			return fmt.Errorf("OVS port %s belongs to another instance", device)
		}
		return nil
	} else if err != client.ErrNotFound {
		return err
	}
	i := &Interface{UUID: "new_iface", Name: device, ExternalIDs: map[string]string{"iface-id": logicalPort, "netlab.instance": instanceID}}
	p = Port{UUID: "new_port", Name: device, Interfaces: []string{i.UUID}, ExternalIDs: map[string]string{"netlab.environment": environmentID, "netlab.asset": assetID, "netlab.instance": instanceID}}
	ops, err := n.client.Create(i, &p)
	if err != nil {
		return err
	}
	b := &Bridge{Name: n.bridge}
	more, err := n.client.Where(b).Mutate(b, model.Mutation{Field: &b.Ports, Mutator: ovsdb.MutateOperationInsert, Value: []string{p.UUID}})
	if err != nil {
		return err
	}
	return transact(ctx, n.client, append(ops, more...))
}
func (n *OVS) Detach(ctx context.Context, device, environmentID, assetID, instanceID string) (bool, error) {
	p := &Port{Name: device}
	if err := n.client.Get(ctx, p); err == client.ErrNotFound {
		return true, nil
	} else if err != nil {
		return false, err
	}
	if p.ExternalIDs["netlab.environment"] != environmentID || p.ExternalIDs["netlab.asset"] != assetID {
		return false, fmt.Errorf("OVS port %s belongs to another asset", device)
	}
	if p.ExternalIDs["netlab.instance"] != instanceID {
		return false, nil
	}
	b := &Bridge{Name: n.bridge}
	ops, err := n.client.Where(b).Mutate(b, model.Mutation{Field: &b.Ports, Mutator: ovsdb.MutateOperationDelete, Value: []string{p.UUID}})
	if err != nil {
		return false, err
	}
	more, err := n.client.Where(p).Delete()
	if err != nil {
		return false, err
	}
	err = transact(ctx, n.client, append(ops, more...))
	return err == nil, err
}
func objectName(prefix, env, id string) string {
	return prefix + "_" + strings.ReplaceAll(env, "-", "") + "_" + strings.ReplaceAll(id, "-", "")
}
