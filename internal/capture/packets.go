package capture

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcapgo"
	"netlab.local/core/api"
)

type PacketQuery struct {
	After                         int64
	Limit                         int
	Source, Destination, Protocol string
	SourcePort, DestinationPort   *int
	InterfaceIndex                *int
}

func ParsePacketQuery(values url.Values) (PacketQuery, error) {
	q := PacketQuery{Limit: 100, Source: values.Get("source"), Destination: values.Get("destination"), Protocol: values.Get("protocol")}
	if value := values.Get("after"); value != "" {
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 0 {
			return q, fmt.Errorf("after 应为非负帧编号")
		}
		q.After = n
	}
	if value := values.Get("limit"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 200 {
			return q, fmt.Errorf("limit 范围为 1–200")
		}
		q.Limit = n
	}
	if value := values.Get("interfaceIndex"); value != "" {
		index, err := strconv.Atoi(value)
		if err != nil || index < 0 {
			return q, fmt.Errorf("interfaceIndex 应为非负接口编号")
		}
		q.InterfaceIndex = &index
	}
	for _, field := range []struct {
		name   string
		target **int
	}{{"sourcePort", &q.SourcePort}, {"destinationPort", &q.DestinationPort}} {
		if value := values.Get(field.name); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 || n > 65535 {
				return q, fmt.Errorf("%s 范围为 0–65535", field.name)
			}
			*field.target = &n
		}
	}
	return q, nil
}

func (q PacketQuery) matches(packet api.CapturePacket) bool {
	if q.InterfaceIndex != nil && packet.InterfaceIndex != *q.InterfaceIndex {
		return false
	}
	if q.Protocol != "" && !strings.EqualFold(q.Protocol, packet.Protocol) {
		return false
	}
	match := func(source, destination string, sp, dp int) bool {
		return (q.Source == "" || q.Source == source) && (q.Destination == "" || q.Destination == destination) &&
			(q.SourcePort == nil || *q.SourcePort == sp) && (q.DestinationPort == nil || *q.DestinationPort == dp)
	}
	return match(packet.Source, packet.Destination, packet.SourcePort, packet.DestinationPort) || match(packet.Destination, packet.Source, packet.DestinationPort, packet.SourcePort)
}

func readFrames(ctx context.Context, input io.Reader, live bool, visit func(api.CapturePacket, []byte) bool) error {
	reader, err := pcapgo.NewNgReader(input, pcapgo.NgReaderOptions{WantMixedLinkType: true})
	if err != nil {
		if live && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
			return nil
		}
		return err
	}
	for number := int64(1); ; number++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, info, err := reader.ReadPacketData()
		if errors.Is(err, io.EOF) || live && errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		if err != nil {
			return err
		}
		packet := summarizePacket(number, data, info, info.AncillaryData[0].(layers.LinkType))
		if !visit(packet, data) {
			return nil
		}
	}
}

func summarizePacket(number int64, data []byte, info gopacket.CaptureInfo, link layers.LinkType) api.CapturePacket {
	result := api.CapturePacket{Number: number, Time: info.Timestamp.UTC(), Length: info.Length, CapturedLength: info.CaptureLength, InterfaceIndex: info.InterfaceIndex, Protocol: link.String()}
	transport := ""
	packet := gopacket.NewPacket(data, link, gopacket.DecodeOptions{Lazy: true, NoCopy: true})
	if value := packet.Layer(layers.LayerTypeEthernet); value != nil {
		eth := value.(*layers.Ethernet)
		result.Source, result.Destination, result.Protocol = eth.SrcMAC.String(), eth.DstMAC.String(), eth.EthernetType.String()
		source, destination := eth.SrcMAC.String(), eth.DstMAC.String()
		result.SourceMac, result.DestinationMac = &source, &destination
	}
	if value := packet.Layer(layers.LayerTypeIPv4); value != nil {
		ip := value.(*layers.IPv4)
		result.Source, result.Destination, result.Protocol = ip.SrcIP.String(), ip.DstIP.String(), ip.Protocol.String()
	}
	if value := packet.Layer(layers.LayerTypeIPv6); value != nil {
		ip := value.(*layers.IPv6)
		result.Source, result.Destination, result.Protocol = ip.SrcIP.String(), ip.DstIP.String(), ip.NextHeader.String()
	}
	if value := packet.Layer(layers.LayerTypeTCP); value != nil {
		transport = "TCP"
		tcp := value.(*layers.TCP)
		result.Protocol = "TCP"
		result.SourcePort, result.DestinationPort = int(tcp.SrcPort), int(tcp.DstPort)
		flags := []string{}
		for _, flag := range []struct {
			enabled bool
			name    string
		}{{tcp.SYN, "SYN"}, {tcp.ACK, "ACK"}, {tcp.FIN, "FIN"}, {tcp.RST, "RST"}, {tcp.PSH, "PSH"}} {
			if flag.enabled {
				flags = append(flags, flag.name)
			}
		}
		result.Summary = fmt.Sprintf("%d → %d [%s] Seq=%d Ack=%d Len=%d", tcp.SrcPort, tcp.DstPort, strings.Join(flags, ", "), tcp.Seq, tcp.Ack, len(tcp.Payload))
	} else if value := packet.Layer(layers.LayerTypeUDP); value != nil {
		transport = "UDP"
		udp := value.(*layers.UDP)
		result.Protocol = "UDP"
		result.SourcePort, result.DestinationPort = int(udp.SrcPort), int(udp.DstPort)
		result.Summary = fmt.Sprintf("%d → %d Len=%d", udp.SrcPort, udp.DstPort, len(udp.Payload))
	} else if value := packet.Layer(layers.LayerTypeICMPv4); value != nil {
		transport = "ICMP"
		result.Summary = value.(*layers.ICMPv4).TypeCode.String()
	} else if value := packet.Layer(layers.LayerTypeICMPv6); value != nil {
		transport = "ICMPv6"
		result.Summary = value.(*layers.ICMPv6).TypeCode.String()
	} else if value := packet.Layer(layers.LayerTypeARP); value != nil {
		arp := value.(*layers.ARP)
		result.Summary = fmt.Sprintf("%s → %s", net.IP(arp.SourceProtAddress), net.IP(arp.DstProtAddress))
	}
	if transport != "" {
		result.TransportProtocol = &transport
	}
	return result
}
