//go:build linux

package network

import (
	"net/netip"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
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

func applyAccessFilter(record *accessRecord) error {
	c := &nftables.Conn{}
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
	c.AddRule(&nftables.Rule{Table: table, Chain: input, Exprs: append(vpnInterface(expr.MetaKeyIIFNAME, "lo"), &expr.Verdict{Kind: expr.VerdictAccept})})
	c.AddRule(&nftables.Rule{Table: table, Chain: input, Exprs: append(vpnInterface(expr.MetaKeyIIFNAME, "access0"),
		&expr.Ct{Register: 1, Key: expr.CtKeySTATE},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: binaryutil.NativeEndian.PutUint32(expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED), Xor: make([]byte, 4)},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: make([]byte, 4)},
		&expr.Verdict{Kind: expr.VerdictAccept},
	)})
	// NDP is scoped to the OVN transit link; tunneled traffic cannot reach namespace services.
	c.AddRule(&nftables.Rule{Table: table, Chain: input, Exprs: append(vpnInterface(expr.MetaKeyIIFNAME, "access0"),
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_ICMPV6}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 0, Len: 1},
		&expr.Cmp{Op: expr.CmpOpGte, Register: 1, Data: []byte{133}},
		&expr.Cmp{Op: expr.CmpOpLte, Register: 1, Data: []byte{136}},
		&expr.Verdict{Kind: expr.VerdictAccept},
	)})
	filter := c.AddChain(&nftables.Chain{Name: "authorize", Table: table, Type: nftables.ChainTypeFilter, Hooknum: nftables.ChainHookPrerouting, Priority: nftables.ChainPriorityMangle})
	nat := c.AddChain(&nftables.Chain{Name: "translate", Table: table, Type: nftables.ChainTypeNAT, Hooknum: nftables.ChainHookPrerouting, Priority: nftables.ChainPriorityNATDest})
	forward := c.AddChain(&nftables.Chain{Name: "forward", Table: table, Type: nftables.ChainTypeFilter, Hooknum: nftables.ChainHookForward, Priority: nftables.ChainPriorityFilter, Policy: &drop})
	for _, peer := range record.Peers {
		for _, route := range peer.Peer.Routes {
			access, destination := netip.MustParsePrefix(route.AccessCidr), netip.MustParsePrefix(route.Cidr)
			for _, value := range peer.Addresses {
				source := netip.MustParsePrefix(value)
				if source.Addr().Is4() != access.Addr().Is4() {
					continue
				}
				match := append(vpnInterface(expr.MetaKeyIIFNAME, "wg0"), vpnPrefix(source, true)...)
				match = append(match, vpnPrefix(access, false)...)
				c.AddRule(&nftables.Rule{Table: table, Chain: filter, Exprs: append(match, &expr.Verdict{Kind: expr.VerdictAccept})})
				if access != destination {
					match := append(vpnInterface(expr.MetaKeyIIFNAME, "wg0"), vpnPrefix(source, true)...)
					match = append(match, vpnPrefix(access, false)...)
					c.AddRule(&nftables.Rule{Table: table, Chain: nat, Exprs: append(match, vpnDNAT(access, destination)...)})
				}
				match = append(vpnInterface(expr.MetaKeyIIFNAME, "wg0"), vpnInterface(expr.MetaKeyOIFNAME, "access0")...)
				match = append(match, vpnPrefix(source, true)...)
				match = append(match, vpnPrefix(destination, false)...)
				c.AddRule(&nftables.Rule{Table: table, Chain: forward, Exprs: append(match, &expr.Verdict{Kind: expr.VerdictAccept})})
			}
		}
	}
	c.AddRule(&nftables.Rule{Table: table, Chain: filter, Exprs: append(vpnInterface(expr.MetaKeyIIFNAME, "wg0"), &expr.Verdict{Kind: expr.VerdictDrop})})
	reply := append(vpnInterface(expr.MetaKeyIIFNAME, "access0"), vpnInterface(expr.MetaKeyOIFNAME, "wg0")...)
	c.AddRule(&nftables.Rule{Table: table, Chain: forward, Exprs: append(reply,
		&expr.Ct{Register: 1, Key: expr.CtKeySTATE},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: binaryutil.NativeEndian.PutUint32(expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED), Xor: make([]byte, 4)},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: make([]byte, 4)},
		&expr.Verdict{Kind: expr.VerdictAccept},
	)})
	post := c.AddChain(&nftables.Chain{Name: "source", Table: table, Type: nftables.ChainTypeNAT, Hooknum: nftables.ChainHookPostrouting, Priority: nftables.ChainPriorityNATSource})
	c.AddRule(&nftables.Rule{Table: table, Chain: post, Exprs: append(vpnInterface(expr.MetaKeyOIFNAME, "access0"), &expr.Masq{})})
	return c.Flush()
}
