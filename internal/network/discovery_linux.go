//go:build linux

package network

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/afpacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/vishvananda/netlink"
	"netlab.local/core/api"
)

func (e *External) Discover(ctx context.Context, input api.ExternalDiscovery) ([]api.DiscoveredDevice, error) {
	prefix, err := discoveryPrefix(input)
	if err != nil {
		return nil, err
	}
	interfaces, err := e.Interfaces(ctx)
	if err != nil {
		return nil, err
	}
	index := slices.IndexFunc(interfaces, func(item api.ExternalInterface) bool { return item.Name == input.Interface && item.Available })
	if index < 0 {
		return nil, fmt.Errorf("请选择可用的接入网口")
	}
	if !interfaces[index].LinkUp {
		return nil, fmt.Errorf("接入网口未连接，请检查网线和上联网络")
	}
	link, err := netlink.LinkByName(input.Interface)
	if err != nil {
		return nil, err
	}
	return discoverARP(ctx, input, prefix, link.Attrs().HardwareAddr)
}

func discoveryPrefix(input api.ExternalDiscovery) (netip.Prefix, error) {
	prefix, err := netip.ParsePrefix(input.Cidr)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() < 22 || !prefix.Addr().IsGlobalUnicast() {
		return netip.Prefix{}, fmt.Errorf("请指定 IPv4 发现范围，单次不超过 1024 个地址（/22）")
	}
	if input.Interface == "" || input.Vlan != nil && (*input.Vlan < 1 || *input.Vlan > 4094) {
		return netip.Prefix{}, fmt.Errorf("接入网口或 VLAN 无效")
	}
	return prefix.Masked(), nil
}

func discoverARP(ctx context.Context, input api.ExternalDiscovery, prefix netip.Prefix, mac net.HardwareAddr) ([]api.DiscoveredDevice, error) {
	packet, err := afpacket.NewTPacket(afpacket.OptInterface(input.Interface), afpacket.OptPollTimeout(50*time.Millisecond), afpacket.OptBlockSize(65536), afpacket.OptNumBlocks(2), afpacket.OptAddVLANHeader(true))
	if err != nil {
		return nil, fmt.Errorf("打开接入网口：%w", err)
	}
	defer packet.Close()
	count := 1 << (32 - prefix.Bits())
	scan, cancel := context.WithTimeout(ctx, time.Duration(count)*4*time.Millisecond+time.Second)
	defer cancel()
	sent := make(chan error, 1)
	go func() {
		tick := time.NewTicker(2 * time.Millisecond)
		defer tick.Stop()
		for round := 0; round < 2; round++ {
			for address := prefix.Addr(); prefix.Contains(address); address = address.Next() {
				if prefix.Bits() < 31 && (address == prefix.Addr() || !prefix.Contains(address.Next())) {
					continue
				}
				select {
				case <-scan.Done():
					sent <- scan.Err()
					return
				case <-tick.C:
				}
				frame, err := discoveryRequest(mac, address, input.Vlan)
				if err == nil {
					err = packet.WritePacketData(frame)
				}
				if err != nil {
					sent <- err
					cancel()
					return
				}
			}
		}
		sent <- nil
	}()
	devices := map[string]api.DiscoveredDevice{}
	var readErr error
	for scan.Err() == nil {
		data, _, err := packet.ZeroCopyReadPacketData()
		if errors.Is(err, afpacket.ErrTimeout) {
			continue
		}
		if err != nil {
			readErr = err
			break
		}
		if device, ok := discoveryReply(data, prefix, input.Vlan); ok && device.Mac != mac.String() {
			devices[device.Address+"/"+device.Mac] = device
		}
	}
	cancel()
	sendErr := <-sent
	if readErr != nil {
		return nil, fmt.Errorf("读取设备发现响应：%w", readErr)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if sendErr != nil {
		return nil, fmt.Errorf("发送设备发现请求：%w", sendErr)
	}
	result := make([]api.DiscoveredDevice, 0, len(devices))
	for _, device := range devices {
		result = append(result, device)
	}
	slices.SortFunc(result, func(a, b api.DiscoveredDevice) int {
		return netip.MustParseAddr(a.Address).Compare(netip.MustParseAddr(b.Address))
	})
	return result, nil
}

func discoveryRequest(mac net.HardwareAddr, address netip.Addr, vlan *int) ([]byte, error) {
	ethernet := &layers.Ethernet{SrcMAC: mac, DstMAC: net.HardwareAddr{255, 255, 255, 255, 255, 255}, EthernetType: layers.EthernetTypeARP}
	arp := &layers.ARP{AddrType: layers.LinkTypeEthernet, Protocol: layers.EthernetTypeIPv4, HwAddressSize: 6, ProtAddressSize: 4, Operation: layers.ARPRequest, SourceHwAddress: mac, SourceProtAddress: []byte{0, 0, 0, 0}, DstHwAddress: make([]byte, 6), DstProtAddress: address.AsSlice()}
	parts := []gopacket.SerializableLayer{ethernet}
	if vlan != nil {
		ethernet.EthernetType = layers.EthernetTypeDot1Q
		parts = append(parts, &layers.Dot1Q{VLANIdentifier: uint16(*vlan), Type: layers.EthernetTypeARP})
	}
	buffer := gopacket.NewSerializeBuffer()
	err := gopacket.SerializeLayers(buffer, gopacket.SerializeOptions{FixLengths: true}, append(parts, arp)...)
	return buffer.Bytes(), err
}

func discoveryReply(data []byte, prefix netip.Prefix, vlan *int) (api.DiscoveredDevice, bool) {
	packet := gopacket.NewPacket(data, layers.LayerTypeEthernet, gopacket.Default)
	tag, tagged := packet.Layer(layers.LayerTypeDot1Q).(*layers.Dot1Q)
	if (vlan != nil) != tagged || tagged && int(tag.VLANIdentifier) != *vlan {
		return api.DiscoveredDevice{}, false
	}
	arp, ok := packet.Layer(layers.LayerTypeARP).(*layers.ARP)
	if !ok || arp.Operation != layers.ARPReply || arp.Protocol != layers.EthernetTypeIPv4 || len(arp.SourceHwAddress) != 6 || arp.SourceHwAddress[0]&1 != 0 {
		return api.DiscoveredDevice{}, false
	}
	address, ok := netip.AddrFromSlice(arp.SourceProtAddress)
	if !ok || !prefix.Contains(address) || !address.IsGlobalUnicast() || prefix.Bits() < 31 && (address == prefix.Addr() || !prefix.Contains(address.Next())) {
		return api.DiscoveredDevice{}, false
	}
	return api.DiscoveredDevice{Address: address.String(), Mac: net.HardwareAddr(arp.SourceHwAddress).String(), ObservedAt: time.Now().UTC()}, true
}
