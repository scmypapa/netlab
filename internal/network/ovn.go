package network

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	"netlab.local/core/api"
)

// Replace an environment's logical configuration in a single transaction. Stable
// port names keep chassis bindings attached while the surrounding rules change.
func (n *OVN) Apply(ctx context.Context, plan api.NodePlan) error {
	models, err := Compile(plan)
	if err != nil {
		return err
	}
	if plan.Gateway != nil {
		var router *Router
		for _, item := range models {
			if current, ok := item.(*Router); ok {
				router = current
				break
			}
		}
		if router == nil {
			router = &Router{UUID: "router", Name: objectName("lr", plan.EnvironmentId, "gateway"), ExternalIDs: ownership(plan.EnvironmentId)}
			models = append(models, router)
		}
		gateway, err := gatewayModels(plan, router, n.provider, n.chassis)
		if err != nil {
			return err
		}
		models = append(models, gateway...)
	}
	if n.vpnRecord != nil {
		if record := n.vpnRecord(plan.EnvironmentId); record != nil && len(record.Peers) > 0 {
			if err := vpnRangeConflict(*record, plan); err != nil {
				return err
			}
			record.Networks = plan.Spec.Networks
			var router *Router
			for _, item := range models {
				if current, ok := item.(*Router); ok {
					router = current
					break
				}
			}
			if router == nil {
				return fmt.Errorf("VPN environment router is missing")
			}
			models = append(models, vpnModels(plan.EnvironmentId, record, router, n.chassis)...)
		}
	}
	ops, err := n.deleteOperations(ctx, plan.EnvironmentId)
	if err != nil {
		return err
	}
	create, err := n.client.Create(models...)
	if err != nil {
		return err
	}
	return transact(ctx, n.client, append(ops, create...))
}
func (n *OVN) Remove(ctx context.Context, environmentID string) error {
	ops, err := n.deleteOperations(ctx, environmentID)
	if err != nil {
		return err
	}
	return transact(ctx, n.client, ops)
}
func (n *OVN) deleteOperations(ctx context.Context, environmentID string) ([]ovsdb.Operation, error) {
	var ops []ovsdb.Operation
	for _, table := range []string{"Logical_Switch", "Logical_Switch_Port", "DHCP_Options", "Logical_Router", "Logical_Router_Port", "Logical_Router_Static_Route", "ACL", "Load_Balancer", "NAT", "Address_Set"} {
		v, err := ovsdb.NewOvsMap(map[string]string{"netlab.environment": environmentID})
		if err != nil {
			return nil, err
		}
		ops = append(ops, ovsdb.Operation{Op: ovsdb.OperationDelete, Table: table, Where: []ovsdb.Condition{{Column: "external_ids", Function: ovsdb.ConditionIncludes, Value: v}}})
	}
	return ops, nil
}

func Compile(plan api.NodePlan) ([]model.Model, error) {
	var models []model.Model
	portNames := make(map[string]string)
	for _, a := range plan.Assets {
		for _, r := range a.Interfaces {
			portNames[r.Id] = r.PortName
		}
	}
	networks := make(map[string]api.Network, len(plan.Spec.Networks))
	switches := make(map[string]*Switch, len(plan.Spec.Networks))
	router := &Router{UUID: "router", Name: objectName("lr", plan.EnvironmentId, "gateway"), ExternalIDs: ownership(plan.EnvironmentId)}
	for i, nw := range plan.Spec.Networks {
		networks[nw.Id] = nw
		p, err := netip.ParsePrefix(nw.Cidr)
		if err != nil {
			return nil, fmt.Errorf("network %s: %w", nw.Name, err)
		}
		sw := &Switch{UUID: fmt.Sprintf("sw%d", i), Name: objectName("ls", plan.EnvironmentId, nw.Id), ExternalIDs: ownership(plan.EnvironmentId)}
		switches[nw.Id] = sw
		models = append(models, sw)
		if nw.Gateway != nil && *nw.Gateway != "" {
			gw, err := netip.ParseAddr(*nw.Gateway)
			if err != nil {
				return nil, err
			}
			mac := routerMAC(gw)
			rp := &RouterPort{UUID: fmt.Sprintf("rp%d", i), Name: objectName("lrp", plan.EnvironmentId, nw.Id), MAC: mac, Networks: []string{netip.PrefixFrom(gw, p.Bits()).String()}, ExternalIDs: ownership(plan.EnvironmentId)}
			if p.Addr().Is6() {
				rp.IPv6RA = map[string]string{"address_mode": "dhcpv6_stateful", "send_periodic": "true"}
			}
			lp := &SwitchPort{UUID: fmt.Sprintf("gw%d", i), Name: objectName("gw", plan.EnvironmentId, nw.Id), Type: "router", Addresses: []string{"router"}, Options: map[string]string{"router-port": rp.Name}, ExternalIDs: ownership(plan.EnvironmentId)}
			sw.Ports = append(sw.Ports, lp.UUID)
			router.Ports = append(router.Ports, rp.UUID)
			models = append(models, rp, lp)
		}
	}
	// The whole specification is supplied even when only some assets execute here.
	// Port identity is consequently independent of node placement.
	for ai, asset := range plan.Spec.Assets {
		for ii, iface := range asset.Interfaces {
			nw, ok := networks[iface.NetworkId]
			if !ok {
				return nil, fmt.Errorf("interface %s references absent network", iface.Id)
			}
			prefix, _ := netip.ParsePrefix(nw.Cidr)
			ip, err := netip.ParseAddr(iface.Address)
			if err != nil {
				return nil, err
			}
			if !prefix.Contains(ip) {
				return nil, fmt.Errorf("interface %s address outside network", iface.Id)
			}
			if _, err = net.ParseMAC(iface.Mac); err != nil {
				return nil, err
			}
			name := portNames[iface.Id]
			if name == "" {
				return nil, fmt.Errorf("interface %s has no resolved port identity", iface.Id)
			}
			uid := fmt.Sprintf("port%d_%d", ai, ii)
			port := &SwitchPort{UUID: uid, Name: name, Addresses: []string{iface.Mac + " " + iface.Address}, PortSecurity: []string{iface.Mac + " " + iface.Address}, ExternalIDs: ownership(plan.EnvironmentId)}
			port.ExternalIDs["netlab.asset"] = asset.Id
			port.ExternalIDs["netlab.interface"] = iface.Id
			options := map[string]string{}
			if ip.Is4() {
				server := prefix.Addr().Next()
				if nw.Gateway != nil && *nw.Gateway != "" {
					server, _ = netip.ParseAddr(*nw.Gateway)
				}
				options["server_id"] = server.String()
				options["server_mac"] = routerMAC(server)
				options["lease_time"] = "3600"
				options["mtu"] = "1400"
				if nw.Mtu != nil {
					options["mtu"] = strconv.Itoa(*nw.Mtu)
				}
				if iface.Primary && nw.Gateway != nil && *nw.Gateway != "" {
					options["router"] = *nw.Gateway
				}
			} else {
				options["server_id"] = routerMAC(prefix.Addr().Next())
			}
			dns := networkDNS(nw, plan.Spec.Assets)
			if len(dns) > 0 {
				options["dns_server"] = "{" + strings.Join(dns, ", ") + "}"
			}
			dhcp := &DHCP{UUID: "dhcp" + uid, CIDR: nw.Cidr, Options: options, ExternalIDs: ownership(plan.EnvironmentId)}
			if ip.Is4() {
				port.DHCPv4 = &dhcp.UUID
			} else {
				port.DHCPv6 = &dhcp.UUID
			}
			switches[nw.Id].Ports = append(switches[nw.Id].Ports, port.UUID)
			models = append(models, dhcp, port)
		}
	}
	if plan.Spec.Routes != nil {
		for i, r := range *plan.Spec.Routes {
			if _, ok := networks[r.NetworkId]; !ok {
				return nil, fmt.Errorf("route references absent network")
			}
			if _, err := netip.ParsePrefix(r.Destination); err != nil {
				return nil, err
			}
			if _, err := netip.ParseAddr(r.NextHop); err != nil {
				return nil, err
			}
			out := objectName("lrp", plan.EnvironmentId, r.NetworkId)
			item := &Route{UUID: fmt.Sprintf("route%d", i), Prefix: r.Destination, NextHop: r.NextHop, OutputPort: &out, ExternalIDs: ownership(plan.EnvironmentId)}
			router.Routes = append(router.Routes, item.UUID)
			models = append(models, item)
		}
	}
	if len(router.Ports) > 0 {
		models = append(models, router)
	}
	if plan.Spec.Policies != nil {
		for pi, p := range *plan.Spec.Policies {
			if p.Action == api.Shape {
				continue
			}
			sw, ok := switches[p.NetworkId]
			if !ok {
				return nil, fmt.Errorf("policy references absent network")
			}
			for di, d := range policyDirections(p.Direction) {
				match, err := policyMatch(p)
				if err != nil {
					return nil, err
				}
				acl := &ACL{UUID: fmt.Sprintf("acl%d_%d", pi, di), Direction: d, Priority: 2000 + pi, Match: match, Action: "allow-related", ExternalIDs: ownership(plan.EnvironmentId)}
				if p.Action == api.Deny {
					acl.Action = "drop"
				}
				sw.ACLs = append(sw.ACLs, acl.UUID)
				models = append(models, acl)
			}
		}
	}
	return models, nil
}
func ownership(env string) map[string]string { return map[string]string{"netlab.environment": env} }
func routerMAC(ip netip.Addr) string {
	b := ip.As16()
	return fmt.Sprintf("02:00:%02x:%02x:%02x:%02x", b[12], b[13], b[14], b[15])
}
func networkDNS(n api.Network, assets []api.Asset) []string {
	if n.DnsAssetId != nil && *n.DnsAssetId != "" {
		for _, a := range assets {
			if a.Id == *n.DnsAssetId {
				for _, i := range a.Interfaces {
					if i.NetworkId == n.Id {
						return []string{i.Address}
					}
				}
			}
		}
	}
	if n.DnsServers != nil {
		return *n.DnsServers
	}
	return nil
}
func policyDirections(direction api.PolicyDirection) []string {
	if direction == api.Ingress {
		return []string{"to-lport"}
	}
	if direction == api.Egress {
		return []string{"from-lport"}
	}
	return []string{"to-lport", "from-lport"}
}
func policyMatch(p api.Policy) (string, error) {
	terms := []string{"ip"}
	if p.Protocol != nil && *p.Protocol != "" {
		switch *p.Protocol {
		case "tcp", "udp", "icmp", "icmp6":
			terms = append(terms, *p.Protocol)
		default:
			return "", fmt.Errorf("invalid ACL protocol %s", *p.Protocol)
		}
	}
	for name, value := range map[string]*string{"src": p.Source, "dst": p.Destination} {
		if value != nil && *value != "" {
			var addr netip.Addr
			if prefix, err := netip.ParsePrefix(*value); err == nil {
				addr = prefix.Addr()
			} else {
				addr, err = netip.ParseAddr(*value)
				if err != nil {
					return "", err
				}
			}
			family := "ip4"
			if addr.Is6() {
				family = "ip6"
			}
			terms = append(terms, family+"."+name+" == "+*value)
		}
	}
	return strings.Join(terms, " && "), nil
}
