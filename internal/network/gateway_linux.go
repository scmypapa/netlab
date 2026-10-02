//go:build linux

package network

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"time"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"netlab.local/core/api"
)

type servicePort struct {
	protocol uint8
	port     uint16
}

func bindingPort(binding api.NodeServiceBinding) (servicePort, error) {
	port := servicePort{port: uint16(binding.ListenPort)}
	switch binding.Protocol {
	case api.Tcp:
		port.protocol = unix.IPPROTO_TCP
	case api.Udp:
		port.protocol = unix.IPPROTO_UDP
	default:
		return port, fmt.Errorf("unsupported service protocol %s", binding.Protocol)
	}
	if binding.ListenPort < 0 || binding.ListenPort > 65535 || binding.TargetPort < 1 || binding.TargetPort > 65535 {
		return port, fmt.Errorf("service %s has an invalid port", binding.Id)
	}
	address, err := netip.ParseAddr(binding.TargetAddress)
	if err != nil || !address.Is4() {
		return port, fmt.Errorf("service %s requires an IPv4 target address", binding.Id)
	}
	return port, nil
}

// Bind without Listen/Accept: the socket reserves a host port, nftables carries the traffic.
func reservePort(port servicePort) (int, servicePort, error) {
	kind := unix.SOCK_STREAM
	if port.protocol == unix.IPPROTO_UDP {
		kind = unix.SOCK_DGRAM
	}
	fd, err := unix.Socket(unix.AF_INET, kind|unix.SOCK_CLOEXEC, int(port.protocol))
	if err != nil {
		return -1, port, err
	}
	if err = unix.Bind(fd, &unix.SockaddrInet4{Port: int(port.port)}); err != nil {
		unix.Close(fd)
		return -1, port, fmt.Errorf("host port %d/%d is occupied: %w", port.port, port.protocol, err)
	}
	address, err := unix.Getsockname(fd)
	if err != nil {
		unix.Close(fd)
		return -1, port, err
	}
	port.port = uint16(address.(*unix.SockaddrInet4).Port)
	return fd, port, nil
}

func configureProvider(ctx context.Context, ovs *OVS, prefix netip.Prefix, nodeID string) (string, error) {
	updates := make(chan netlink.LinkUpdate, 8)
	done := make(chan struct{})
	if err := netlink.LinkSubscribe(updates, done); err != nil {
		return "", err
	}
	defer close(done)
	chassis, err := ovs.EnsureProvider(ctx, nodeID)
	if err != nil {
		return "", err
	}
	deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var link netlink.Link
	for {
		link, err = netlink.LinkByName(ProviderBridge)
		var missing netlink.LinkNotFoundError
		if !errors.As(err, &missing) {
			break
		}
		select {
		case <-deadline.Done():
			return "", fmt.Errorf("OVS provider interface: %w", deadline.Err())
		case <-updates:
		}
	}
	if err != nil {
		return "", err
	}
	address := &netlink.Addr{IPNet: &net.IPNet{IP: net.IP(prefix.Addr().Next().AsSlice()), Mask: net.CIDRMask(prefix.Bits(), 32)}}
	if err = netlink.AddrReplace(link, address); err != nil {
		return "", err
	}
	if err = netlink.LinkSetUp(link); err != nil {
		return "", err
	}
	// Localhost publication returns through this bridge after reverse SNAT.
	for _, path := range []string{"/proc/sys/net/ipv4/ip_forward", "/proc/sys/net/ipv4/conf/" + ProviderBridge + "/route_localnet"} {
		if err = os.WriteFile(path, []byte("1"), 0644); err != nil {
			return "", err
		}
	}
	return chassis, nil
}

type kernelBinding struct {
	port    servicePort
	address netip.Addr
}

func portKey(port servicePort) []byte {
	key := make([]byte, 8)
	key[0] = port.protocol
	binary.BigEndian.PutUint16(key[4:6], port.port)
	return key
}

func interfaceMatch(key expr.MetaKey) []expr.Any {
	name := make([]byte, 16)
	copy(name, ProviderBridge)
	return []expr.Any{&expr.Meta{Key: key, Register: unix.NFT_REG32_00}, &expr.Cmp{Op: expr.CmpOpEq, Register: unix.NFT_REG32_00, Data: name}}
}

// Both NAT and filtering consume the same active binding set; stale conntrack cannot bypass withdrawal.
func applyKernelBindings(tableName string, bridgeAddress netip.Addr, bindings []kernelBinding, gatewayAddresses []netip.Addr) error {
	c := &nftables.Conn{}
	tables, err := c.ListTables()
	if err != nil {
		return err
	}
	for _, table := range tables {
		if table.Name == tableName && table.Family == nftables.TableFamilyIPv4 {
			c.DelTable(table)
		}
	}
	table := c.AddTable(&nftables.Table{Name: tableName, Family: nftables.TableFamilyIPv4})
	natMap := &nftables.Set{Table: table, Name: "ports", ID: 1, IsMap: true, Concatenation: true, KeyType: nftables.MustConcatSetType(nftables.TypeInetProto, nftables.TypeInetService), DataType: nftables.TypeIPAddr}
	active := &nftables.Set{Table: table, Name: "active", ID: 2, Concatenation: true, KeyType: nftables.MustConcatSetType(nftables.TypeIPAddr, nftables.TypeInetProto, nftables.TypeInetService)}
	gateways := &nftables.Set{Table: table, Name: "gateways", ID: 3, KeyType: nftables.TypeIPAddr}
	var targets, allowed []nftables.SetElement
	for _, binding := range bindings {
		key := portKey(binding.port)
		address := binding.address.As4()
		targets = append(targets, nftables.SetElement{Key: key, Val: address[:]})
		allowed = append(allowed, nftables.SetElement{Key: append(address[:], key...)})
	}
	if err = c.AddSet(natMap, targets); err != nil {
		return err
	}
	if err = c.AddSet(active, allowed); err != nil {
		return err
	}
	var gatewayElements []nftables.SetElement
	for _, address := range gatewayAddresses {
		gatewayElements = append(gatewayElements, nftables.SetElement{Key: address.AsSlice()})
	}
	if err = c.AddSet(gateways, gatewayElements); err != nil {
		return err
	}
	for _, hook := range []*nftables.ChainHook{nftables.ChainHookPrerouting, nftables.ChainHookOutput} {
		name := "prerouting"
		if hook == nftables.ChainHookOutput {
			name = "output"
		}
		chain := c.AddChain(&nftables.Chain{Name: name, Table: table, Type: nftables.ChainTypeNAT, Hooknum: hook, Priority: nftables.ChainPriorityNATDest})
		c.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: []expr.Any{
			&expr.Fib{Register: unix.NFT_REG32_00, ResultADDRTYPE: true, FlagDADDR: true},
			&expr.Cmp{Op: expr.CmpOpEq, Register: unix.NFT_REG32_00, Data: binaryutil.NativeEndian.PutUint32(unix.RTN_LOCAL)},
			&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: unix.NFT_REG32_00},
			&expr.Payload{DestRegister: unix.NFT_REG32_01, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
			&expr.Lookup{SourceRegister: unix.NFT_REG32_00, DestRegister: unix.NFT_REG32_00, IsDestRegSet: true, SetName: natMap.Name, SetID: natMap.ID},
			&expr.NAT{Type: expr.NATTypeDestNAT, Family: unix.NFPROTO_IPV4, RegAddrMin: unix.NFT_REG32_00},
		}})
	}
	postrouting := c.AddChain(&nftables.Chain{Name: "postrouting", Table: table, Type: nftables.ChainTypeNAT, Hooknum: nftables.ChainHookPostrouting, Priority: nftables.ChainPriorityNATSource})
	c.AddRule(&nftables.Rule{Table: table, Chain: postrouting, Exprs: append(interfaceMatch(expr.MetaKeyOIFNAME),
		&expr.Payload{DestRegister: unix.NFT_REG32_00, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Lookup{SourceRegister: unix.NFT_REG32_00, SetName: gateways.Name, SetID: gateways.ID},
		&expr.Immediate{Register: unix.NFT_REG32_00, Data: bridgeAddress.AsSlice()},
		&expr.NAT{Type: expr.NATTypeSourceNAT, Family: unix.NFPROTO_IPV4, RegAddrMin: unix.NFT_REG32_00},
	)})
	forward := c.AddChain(&nftables.Chain{Name: "forward", Table: table, Type: nftables.ChainTypeFilter, Hooknum: nftables.ChainHookForward, Priority: nftables.ChainPriorityFilter})
	for _, direction := range []struct {
		key                  expr.MetaKey
		ipOffset, portOffset uint32
	}{
		{expr.MetaKeyOIFNAME, 16, 2}, {expr.MetaKeyIIFNAME, 12, 0},
	} {
		match := interfaceMatch(direction.key)
		c.AddRule(&nftables.Rule{Table: table, Chain: forward, Exprs: append(match,
			&expr.Payload{DestRegister: unix.NFT_REG32_00, Base: expr.PayloadBaseNetworkHeader, Offset: direction.ipOffset, Len: 4},
			&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: unix.NFT_REG32_01},
			&expr.Payload{DestRegister: unix.NFT_REG32_02, Base: expr.PayloadBaseTransportHeader, Offset: direction.portOffset, Len: 2},
			&expr.Lookup{SourceRegister: unix.NFT_REG32_00, SetName: active.Name, SetID: active.ID},
			&expr.Verdict{Kind: expr.VerdictAccept},
		)})
		scope := append(interfaceMatch(direction.key),
			&expr.Payload{DestRegister: unix.NFT_REG32_00, Base: expr.PayloadBaseNetworkHeader, Offset: direction.ipOffset, Len: 4},
			&expr.Lookup{SourceRegister: unix.NFT_REG32_00, SetName: gateways.Name, SetID: gateways.ID},
		)
		c.AddRule(&nftables.Rule{Table: table, Chain: forward, Exprs: append(slices.Clone(scope),
			&expr.Ct{Register: unix.NFT_REG32_00, Key: expr.CtKeySTATE},
			&expr.Bitwise{SourceRegister: unix.NFT_REG32_00, DestRegister: unix.NFT_REG32_00, Len: 4, Mask: binaryutil.NativeEndian.PutUint32(expr.CtStateBitRELATED), Xor: make([]byte, 4)},
			&expr.Cmp{Op: expr.CmpOpEq, Register: unix.NFT_REG32_00, Data: binaryutil.NativeEndian.PutUint32(expr.CtStateBitRELATED)},
			&expr.Verdict{Kind: expr.VerdictAccept},
		)})
		c.AddRule(&nftables.Rule{Table: table, Chain: forward, Exprs: append(scope, &expr.Verdict{Kind: expr.VerdictDrop})})
	}
	return c.Flush()
}

// A mapping's original VIP distinguishes its OVN conntrack entries from unrelated guest connections.
type bindingConnections struct {
	gateway netip.Addr
	ports   map[servicePort]bool
	local   map[netip.Addr]bool
}

func (filter bindingConnections) MatchConntrackFlow(flow *netlink.ConntrackFlow) bool {
	if !filter.ports[servicePort{protocol: flow.Forward.Protocol, port: flow.Forward.DstPort}] {
		return false
	}
	address, ok := netip.AddrFromSlice(flow.Forward.DstIP)
	if !ok {
		return false
	}
	address = address.Unmap()
	return address == filter.gateway || (flow.Zone == 0 && (address.IsLoopback() || filter.local[address]))
}

func clearBindingConnections(gateway netip.Addr, ports []servicePort) error {
	if len(ports) == 0 {
		return nil
	}
	addresses, err := netlink.AddrList(nil, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	filter := bindingConnections{gateway: gateway, ports: map[servicePort]bool{}, local: map[netip.Addr]bool{}}
	for _, port := range ports {
		filter.ports[port] = true
	}
	for _, item := range addresses {
		address, _ := netip.AddrFromSlice(item.IP)
		filter.local[address.Unmap()] = true
	}
	_, err = netlink.ConntrackDeleteFilters(netlink.ConntrackTable, netlink.FAMILY_V4, filter)
	return err
}
