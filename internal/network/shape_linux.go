//go:build linux

package network

import (
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
	"netlab.local/core/api"
)

func ifbName(port string) string { return "nf" + strings.ReplaceAll(port, "-", "")[:12] }

func Shape(device, port string, policies []api.Policy) error {
	link, err := netlink.LinkByName(device)
	if err != nil {
		return err
	}
	incoming, outgoing := []api.Policy{}, []api.Policy{}
	for _, p := range policies {
		if p.Action != api.Shape {
			continue
		}
		if p.Direction != api.Egress {
			incoming = append(incoming, p)
		}
		if p.Direction != api.Ingress {
			outgoing = append(outgoing, p)
		}
	}
	if err = shapeOutput(link, incoming); err != nil {
		return err
	}
	if len(outgoing) == 0 {
		return clearInput(link, port)
	}
	name := ifbName(port)
	ifb, err := netlink.LinkByName(name)
	var missing netlink.LinkNotFoundError
	if errors.As(err, &missing) {
		ifb = &netlink.Ifb{LinkAttrs: netlink.LinkAttrs{Name: name, MTU: link.Attrs().MTU}}
		if err = netlink.LinkAdd(ifb); err != nil {
			return err
		}
		ifb, err = netlink.LinkByName(name)
	}
	if err != nil {
		return err
	}
	if err = netlink.LinkSetUp(ifb); err != nil {
		return err
	}
	ingress := &netlink.Ingress{QdiscAttrs: netlink.QdiscAttrs{LinkIndex: link.Attrs().Index, Parent: netlink.HANDLE_INGRESS, Handle: netlink.MakeHandle(0xffff, 0)}}
	if err = netlink.QdiscReplace(ingress); err != nil {
		return err
	}
	filter := &netlink.MatchAll{FilterAttrs: netlink.FilterAttrs{LinkIndex: link.Attrs().Index, Parent: netlink.HANDLE_INGRESS, Priority: 1, Protocol: unix.ETH_P_ALL}, Actions: []netlink.Action{netlink.NewMirredAction(ifb.Attrs().Index)}}
	if err = netlink.FilterReplace(filter); err != nil {
		return err
	}
	return shapeOutput(ifb, outgoing)
}
func ClearShape(device, port string) error {
	link, err := netlink.LinkByName(device)
	var missing netlink.LinkNotFoundError
	if errors.As(err, &missing) {
		ifb, err := netlink.LinkByName(ifbName(port))
		if errors.As(err, &missing) {
			return nil
		}
		if err != nil {
			return err
		}
		return netlink.LinkDel(ifb)
	}
	if err != nil {
		return err
	}
	return errors.Join(shapeOutput(link, nil), clearInput(link, port))
}
func clearInput(link netlink.Link, port string) error {
	qdiscs, err := netlink.QdiscList(link)
	if err != nil {
		return err
	}
	for _, q := range qdiscs {
		if q.Attrs().Parent == netlink.HANDLE_INGRESS {
			if err = netlink.QdiscDel(q); err != nil {
				return err
			}
		}
	}
	ifb, err := netlink.LinkByName(ifbName(port))
	var missing netlink.LinkNotFoundError
	if errors.As(err, &missing) {
		return nil
	}
	if err != nil {
		return err
	}
	return netlink.LinkDel(ifb)
}
func shapeOutput(link netlink.Link, policies []api.Policy) error {
	index := link.Attrs().Index
	old, err := netlink.QdiscList(link)
	if err != nil {
		return err
	}
	for _, q := range old {
		if q.Attrs().Parent == netlink.HANDLE_ROOT && q.Attrs().Handle == netlink.MakeHandle(0x4e4c, 0) {
			if err = netlink.QdiscDel(q); err != nil {
				return err
			}
		}
	}
	if len(policies) == 0 {
		return nil
	}
	root := netlink.MakeHandle(0x4e4c, 0)
	if err = netlink.QdiscReplace(&netlink.GenericQdisc{QdiscAttrs: netlink.QdiscAttrs{LinkIndex: index, Handle: root, Parent: netlink.HANDLE_ROOT}, QdiscType: "drr"}); err != nil {
		return fmt.Errorf("create shaping scheduler: %w", err)
	}
	for i := -1; i < len(policies); i++ {
		classID := netlink.MakeHandle(0x4e4c, uint16(i+2))
		if err = netlink.ClassAdd(&netlink.GenericClass{ClassAttrs: netlink.ClassAttrs{LinkIndex: index, Handle: classID, Parent: root}, ClassType: "drr"}); err != nil {
			return fmt.Errorf("create traffic class: %w", err)
		}
		if i == -1 {
			continue
		}
		p := policies[i]
		attrs := netlink.NetemQdiscAttrs{Limit: 10000}
		if p.DelayMs != nil {
			attrs.Latency = uint32(*p.DelayMs) * 1000
		}
		if p.LossPercent != nil {
			attrs.Loss = float32(*p.LossPercent)
		}
		if p.BandwidthKbps != nil {
			attrs.Rate64 = uint64(*p.BandwidthKbps) * 1000 / 8
		}
		q := netlink.NewNetem(netlink.QdiscAttrs{LinkIndex: index, Handle: netlink.MakeHandle(uint16(i+1), 0), Parent: classID}, attrs)
		if err = netlink.QdiscAdd(q); err != nil {
			return fmt.Errorf("create netem queue: %w", err)
		}
		filters, err := shapeFilters(p, index, root, classID, uint16(2*i+1))
		if err != nil {
			return err
		}
		for _, f := range filters {
			if err = netlink.FilterAdd(f); err != nil {
				return fmt.Errorf("create packet selector protocol %d priority %d: %w", f.Attrs().Protocol, f.Attrs().Priority, err)
			}
		}
	}
	return netlink.FilterAdd(&netlink.MatchAll{FilterAttrs: netlink.FilterAttrs{LinkIndex: index, Parent: root, Priority: 65535, Protocol: unix.ETH_P_ALL}, ClassId: netlink.MakeHandle(0x4e4c, 1)})
}
func shapeFilters(p api.Policy, index int, parent, classID uint32, priority uint16) ([]netlink.Filter, error) {
	filters := []netlink.Filter{}
	for family, eth := range []uint16{unix.ETH_P_IP, unix.ETH_P_IPV6} {
		f := &netlink.Flower{FilterAttrs: netlink.FilterAttrs{LinkIndex: index, Parent: parent, Priority: priority + uint16(family), Protocol: eth}, EthType: eth, ClassId: classID}
		if p.Protocol != nil && *p.Protocol != "" {
			var proto nl.IPProto
			switch *p.Protocol {
			case "tcp":
				proto = nl.IPPROTO_TCP
			case "udp":
				proto = nl.IPPROTO_UDP
			case "icmp":
				if eth == unix.ETH_P_IPV6 {
					continue
				}
				proto = nl.IPPROTO_ICMP
			case "icmp6":
				if eth == unix.ETH_P_IP {
					continue
				}
				proto = nl.IPPROTO_ICMPV6
			default:
				return nil, fmt.Errorf("invalid shape protocol")
			}
			f.IPProto = &proto
		}
		familyMatches := true
		for key, value := range map[string]*string{"src": p.Source, "dst": p.Destination} {
			if value == nil || *value == "" {
				continue
			}
			ip, mask, err := shapeAddress(*value)
			if err != nil {
				return nil, err
			}
			if (ip.To4() != nil) != (eth == unix.ETH_P_IP) {
				familyMatches = false
				break
			}
			if key == "src" {
				f.SrcIP = ip
				f.SrcIPMask = mask
			} else {
				f.DestIP = ip
				f.DestIPMask = mask
			}
		}
		if familyMatches {
			filters = append(filters, f)
		}
	}
	return filters, nil
}
func shapeAddress(value string) (net.IP, net.IPMask, error) {
	if ip, nw, err := net.ParseCIDR(value); err == nil {
		return ip, nw.Mask, nil
	}
	ip := net.ParseIP(value)
	if ip == nil {
		return nil, nil, fmt.Errorf("invalid shape address")
	}
	bits := 128
	if ip.To4() != nil {
		bits = 32
	}
	return ip, net.CIDRMask(bits, bits), nil
}
