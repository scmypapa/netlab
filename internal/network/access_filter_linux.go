//go:build linux

package network

import (
	"encoding/binary"
	"fmt"
	"net/netip"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"
	"netlab.local/core/api"
)

func vpnInterface(key expr.MetaKey, name string) []expr.Any {
	value := make([]byte, 16)
	copy(value, name)
	return []expr.Any{&expr.Meta{Key: key, Register: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: value}}
}

func vpnPrefix(prefix netip.Prefix, source bool) []expr.Any {
	family, offset, length := byte(unix.NFPROTO_IPV4), uint32(16), uint32(4)
	if prefix.Addr().Is6() {
		family, offset, length = unix.NFPROTO_IPV6, 24, 16
	}
	if source {
		offset -= length
	}
	mask := make([]byte, length)
	for bit := 0; bit < prefix.Bits(); bit++ {
		mask[bit/8] |= 1 << (7 - bit%8)
	}
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{family}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: offset, Len: length},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: length, Mask: mask, Xor: make([]byte, length)},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: prefix.Masked().Addr().AsSlice()},
	}
}

func vpnDNAT(access, destination netip.Prefix) []expr.Any {
	family, offset, length := uint32(unix.NFPROTO_IPV4), uint32(16), uint32(4)
	if access.Addr().Is6() {
		family, offset, length = unix.NFPROTO_IPV6, 24, 16
	}
	mask := make([]byte, length)
	for bit := access.Bits(); bit < access.Addr().BitLen(); bit++ {
		mask[bit/8] |= 1 << (7 - bit%8)
	}
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: offset, Len: length},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: length, Mask: mask, Xor: destination.Masked().Addr().AsSlice()},
		&expr.NAT{Type: expr.NATTypeDestNAT, Family: family, RegAddrMin: 1},
	}
}

func accessZone(table uint32) []expr.Any {
	return []expr.Any{&expr.Immediate{Register: 1, Data: binaryutil.NativeEndian.PutUint16(uint16(table))}, &expr.Ct{Register: 1, SourceRegister: true, Key: expr.CtKeyZONE}}
}
func accessMark(table uint32) []expr.Any {
	return []expr.Any{&expr.Immediate{Register: 1, Data: binaryutil.NativeEndian.PutUint32(table)}, &expr.Meta{Key: expr.MetaKeyMARK, Register: 1, SourceRegister: true}}
}
func accessState() []expr.Any {
	return []expr.Any{&expr.Ct{Register: 1, Key: expr.CtKeySTATE}, &expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: binaryutil.NativeEndian.PutUint32(expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED), Xor: make([]byte, 4)}, &expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: make([]byte, 4)}}
}
func serviceMatch(service api.NodeServiceBinding, port int) []expr.Any {
	protocol := byte(unix.IPPROTO_TCP)
	if service.Protocol == api.Udp {
		protocol = unix.IPPROTO_UDP
	}
	value := make([]byte, 2)
	binary.BigEndian.PutUint16(value, uint16(port))
	return []expr.Any{&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{protocol}}, &expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: value}}
}
func applyAccessFilter(environment string, record *accessRecord, services gatewayRecord) error {
	links, err := accessLinks(environment, record)
	if err != nil {
		return err
	}
	rules := 0
	// Each ACK consumes an skb. Size both queues for the atomic batch rather
	// than relying on the host's small default socket buffers.
	c, err := nftables.New(nftables.WithSockOptions(func(conn *netlink.Conn) error {
		size := (rules + 16) * 4096
		if err := conn.SetReadBuffer(size); err != nil {
			return err
		}
		return conn.SetWriteBuffer(size)
	}))
	if err != nil {
		return err
	}
	tables, err := c.ListTables()
	if err != nil {
		return err
	}
	for _, table := range tables {
		if table.Name == "netlab_vpn" && table.Family == nftables.TableFamilyINet {
			c.DelTable(table)
		}
	}
	table := c.AddTable(&nftables.Table{Name: "netlab_vpn", Family: nftables.TableFamilyINet})
	drop := nftables.ChainPolicyDrop
	input := c.AddChain(&nftables.Chain{Name: "input", Table: table, Type: nftables.ChainTypeFilter, Hooknum: nftables.ChainHookInput, Priority: nftables.ChainPriorityFilter, Policy: &drop})
	forward := c.AddChain(&nftables.Chain{Name: "forward", Table: table, Type: nftables.ChainTypeFilter, Hooknum: nftables.ChainHookForward, Priority: nftables.ChainPriorityFilter, Policy: &drop})
	raw := c.AddChain(&nftables.Chain{Name: "network", Table: table, Type: nftables.ChainTypeFilter, Hooknum: nftables.ChainHookPrerouting, Priority: nftables.ChainPriorityRaw})
	output := c.AddChain(&nftables.Chain{Name: "local_network", Table: table, Type: nftables.ChainTypeFilter, Hooknum: nftables.ChainHookOutput, Priority: nftables.ChainPriorityRaw})
	authorize := c.AddChain(&nftables.Chain{Name: "authorize", Table: table, Type: nftables.ChainTypeFilter, Hooknum: nftables.ChainHookPrerouting, Priority: nftables.ChainPriorityMangle})
	nat := c.AddChain(&nftables.Chain{Name: "translate", Table: table, Type: nftables.ChainTypeNAT, Hooknum: nftables.ChainHookPrerouting, Priority: nftables.ChainPriorityNATDest})
	post := c.AddChain(&nftables.Chain{Name: "source", Table: table, Type: nftables.ChainTypeNAT, Hooknum: nftables.ChainHookPostrouting, Priority: nftables.ChainPriorityNATSource})
	add := func(chain *nftables.Chain, expressions []expr.Any) {
		c.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: expressions})
		rules++
	}
	accept := func(expressions []expr.Any) []expr.Any {
		return append(expressions, &expr.Verdict{Kind: expr.VerdictAccept})
	}
	add(input, accept(vpnInterface(expr.MetaKeyIIFNAME, "lo")))
	for _, link := range links {
		for _, name := range []string{link.device, link.vrf} {
			add(raw, append(vpnInterface(expr.MetaKeyIIFNAME, name), accessZone(link.table)...))
			add(input, accept(append(vpnInterface(expr.MetaKeyIIFNAME, name), accessState()...)))
			ndp := append(vpnInterface(expr.MetaKeyIIFNAME, name), &expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_ICMPV6}}, &expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Len: 1}, &expr.Cmp{Op: expr.CmpOpGte, Register: 1, Data: []byte{133}}, &expr.Cmp{Op: expr.CmpOpLte, Register: 1, Data: []byte{136}})
			add(input, accept(ndp))
			for _, destination := range []string{"wg0", "access0"} {
				match := append(vpnInterface(expr.MetaKeyIIFNAME, name), vpnInterface(expr.MetaKeyOIFNAME, destination)...)
				add(forward, accept(append(match, accessState()...)))
			}
		}
		local := []expr.Any{&expr.Meta{Key: expr.MetaKeyMARK, Register: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.NativeEndian.PutUint32(link.table)}}
		add(output, append(local, accessZone(link.table)...))
		add(post, append(vpnInterface(expr.MetaKeyOIFNAME, link.device), &expr.Masq{}))
	}
	for _, peer := range record.Peers {
		for _, route := range peer.Peer.Routes {
			link, exists := links[route.NetworkId]
			if !exists {
				return fmt.Errorf("VPN network %s has no management link", route.NetworkId)
			}
			access, destination := netip.MustParsePrefix(route.AccessCidr), netip.MustParsePrefix(route.Cidr)
			for _, value := range peer.Addresses {
				source := netip.MustParsePrefix(value)
				if source.Addr().Is4() != access.Addr().Is4() {
					continue
				}
				match := func() []expr.Any {
					return append(append(vpnInterface(expr.MetaKeyIIFNAME, "wg0"), vpnPrefix(source, true)...), vpnPrefix(access, false)...)
				}
				tagged := append(match(), accessZone(link.table)...)
				tagged = append(tagged, accessMark(link.table)...)
				add(raw, accept(tagged))
				add(authorize, accept(match()))
				if access != destination {
					add(nat, append(match(), vpnDNAT(access, destination)...))
				}
				forwarded := append(vpnInterface(expr.MetaKeyIIFNAME, "wg0"), vpnInterface(expr.MetaKeyOIFNAME, link.device)...)
				forwarded = append(forwarded, vpnPrefix(source, true)...)
				forwarded = append(forwarded, vpnPrefix(destination, false)...)
				add(forward, accept(forwarded))
			}
		}
	}
	add(authorize, append(vpnInterface(expr.MetaKeyIIFNAME, "wg0"), &expr.Verdict{Kind: expr.VerdictDrop}))
	for _, service := range services.Services {
		link, exists := links[service.NetworkId]
		if !exists {
			return fmt.Errorf("service %s network has no management link", service.Id)
		}
		target, err := netip.ParseAddr(service.TargetAddress)
		if err != nil || !netip.MustParsePrefix(link.network.Cidr).Contains(target) {
			return fmt.Errorf("service %s target is outside its network", service.Id)
		}
		match := func() []expr.Any {
			return append(vpnInterface(expr.MetaKeyIIFNAME, "access0"), serviceMatch(service, service.ListenPort)...)
		}
		tagged := append(match(), accessZone(link.table)...)
		tagged = append(tagged, accessMark(link.table)...)
		add(raw, accept(tagged))
		port := make([]byte, 2)
		binary.BigEndian.PutUint16(port, uint16(service.TargetPort))
		translation := append(match(), &expr.Immediate{Register: 1, Data: target.AsSlice()}, &expr.Immediate{Register: 2, Data: port}, &expr.NAT{Type: expr.NATTypeDestNAT, Family: unix.NFPROTO_IPV4, RegAddrMin: 1, RegProtoMin: 2})
		add(nat, translation)
		matchForward := append(vpnInterface(expr.MetaKeyIIFNAME, "access0"), vpnInterface(expr.MetaKeyOIFNAME, link.device)...)
		matchForward = append(matchForward, vpnPrefix(netip.PrefixFrom(target, 32), false)...)
		add(forward, accept(append(matchForward, serviceMatch(service, service.TargetPort)...)))
	}
	return c.Flush()
}
