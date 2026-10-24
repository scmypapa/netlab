package capture

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/pcapgo"
	"netlab.local/core/api"
)

type PacketStream struct {
	io.Writer
	Bytes int64
}

func (w *PacketStream) Write(data []byte) (int, error) {
	n, err := w.Writer.Write(data)
	w.Bytes += int64(n)
	return n, err
}
func EmptyPacketCapture(output io.Writer) error {
	writer, err := pcapgo.NewNgWriter(output, 1)
	if err != nil {
		return err
	}
	return writer.Flush()
}

type RecentQuery struct {
	Start, End                                                               time.Time
	Asset, Interface, Network, Source, Destination, Protocol, Filter, Cursor string
	SourcePort, DestinationPort                                              *int
	Limit                                                                    int
}

func ParseRecentQuery(values url.Values) (RecentQuery, error) {
	legacy, err := ParsePacketQuery(values)
	if err != nil {
		return RecentQuery{}, err
	}
	q := RecentQuery{Asset: values.Get("assetId"), Interface: values.Get("interfaceId"), Network: values.Get("networkId"), Source: legacy.Source, Destination: legacy.Destination, Protocol: legacy.Protocol, SourcePort: legacy.SourcePort, DestinationPort: legacy.DestinationPort, Filter: values.Get("filter"), Cursor: values.Get("cursor"), Limit: legacy.Limit}
	for _, item := range []struct {
		name   string
		target *time.Time
	}{{"start", &q.Start}, {"end", &q.End}} {
		if raw := values.Get(item.name); raw != "" {
			*item.target, err = time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				return q, fmt.Errorf("%s应为RFC3339时间", item.name)
			}
		}
	}
	if !q.Start.IsZero() && !q.End.IsZero() && q.End.Before(q.Start) {
		return q, errors.New("结束时间早于开始时间")
	}
	for _, address := range []string{q.Source, q.Destination} {
		if address != "" {
			if _, err = netip.ParseAddr(address); err != nil {
				if _, err = net.ParseMAC(address); err != nil {
					return q, errors.New("来源和目标应为IP或MAC地址")
				}
			}
		}
	}
	if q.Cursor != "" {
		if _, err = parsePacketCursor(q.Cursor); err != nil {
			return q, err
		}
	}
	return q, nil
}
func (q RecentQuery) display(part packetPart) (string, bool) {
	conditions := []string{}
	if !q.Start.IsZero() {
		conditions = append(conditions, fmt.Sprintf("frame.time_epoch >= %.9f", float64(q.Start.UnixNano())/1e9))
	}
	if !q.End.IsZero() {
		conditions = append(conditions, fmt.Sprintf("frame.time_epoch <= %.9f", float64(q.End.UnixNano())/1e9))
	}
	if q.Asset != "" || q.Interface != "" || q.Network != "" {
		interfaces := []string{}
		for index, iface := range part.Interfaces {
			if (q.Asset == "" || q.Asset == iface.AssetId) && (q.Interface == "" || q.Interface == iface.InterfaceId) && (q.Network == "" || q.Network == iface.NetworkId) {
				interfaces = append(interfaces, fmt.Sprintf("frame.interface_id == %d", index))
			}
		}
		if len(interfaces) == 0 {
			return "", false
		}
		conditions = append(conditions, "("+strings.Join(interfaces, " || ")+")")
	}
	endpoint := func(side, address string) string {
		if address == "" {
			return ""
		}
		if ip, err := netip.ParseAddr(address); err == nil {
			prefix := "ip"
			if ip.Is6() {
				prefix = "ipv6"
			}
			return prefix + "." + side + " == " + ip.String()
		}
		mac, _ := net.ParseMAC(address)
		return "eth." + side + " == " + mac.String()
	}
	direction := func(reverse bool) string {
		source, destination, sp, dp := q.Source, q.Destination, q.SourcePort, q.DestinationPort
		if reverse {
			source, destination, sp, dp = destination, source, dp, sp
		}
		fields := []string{}
		for _, field := range []string{endpoint("src", source), endpoint("dst", destination)} {
			if field != "" {
				fields = append(fields, field)
			}
		}
		if sp != nil {
			fields = append(fields, fmt.Sprintf("(tcp.srcport == %d || udp.srcport == %d)", *sp, *sp))
		}
		if dp != nil {
			fields = append(fields, fmt.Sprintf("(tcp.dstport == %d || udp.dstport == %d)", *dp, *dp))
		}
		return strings.Join(fields, " && ")
	}
	if forward := direction(false); forward != "" {
		conditions = append(conditions, "(("+forward+") || ("+direction(true)+"))")
	}
	if q.Protocol != "" {
		protocol := strings.ToLower(q.Protocol)
		if slices.Contains([]string{"tcp", "udp", "icmp", "icmpv6", "arp", "ip", "ipv6", "dns", "http", "tls"}, protocol) {
			conditions = append(conditions, protocol)
		} else {
			conditions = append(conditions, "_ws.col.Protocol == "+strconv.Quote(q.Protocol))
		}
	}
	if q.Filter != "" {
		conditions = append(conditions, "("+q.Filter+")")
	}
	return strings.Join(conditions, " && "), true
}
func packetOrder(a, b api.CapturePacket) int {
	if order := a.Time.Compare(b.Time); order != 0 {
		return order
	}
	return strings.Compare(*a.Id, *b.Id)
}
func MergePacketPages(pages []api.RecentPacketPage, limit int) api.RecentPacketPage {
	result := api.RecentPacketPage{Packets: []api.CapturePacket{}, Errors: map[string]string{}, Retention: api.PacketRetention{LimitBytes: PacketBudget, Status: api.PacketRetentionStatusIdle}}
	more := false
	for _, page := range pages {
		result.Packets = append(result.Packets, page.Packets...)
		more = more || page.NextCursor != nil
		for node, message := range page.Errors {
			result.Errors[node] = message
		}
		mergeRetention(&result.Retention, page.Retention)
	}
	slices.SortFunc(result.Packets, packetOrder)
	if len(result.Packets) > limit {
		more = true
		result.Packets = result.Packets[len(result.Packets)-limit:]
	}
	if more && len(result.Packets) > 0 {
		cursor := packetCursor(result.Packets[0])
		result.NextCursor = &cursor
	}
	return result
}
func mergeRetention(total *api.PacketRetention, part api.PacketRetention) {
	total.UsedBytes += part.UsedBytes
	if part.OldestAt != nil && (total.OldestAt == nil || part.OldestAt.Before(*total.OldestAt)) {
		total.OldestAt = part.OldestAt
	}
	if part.NewestAt != nil && (total.NewestAt == nil || part.NewestAt.After(*total.NewestAt)) {
		total.NewestAt = part.NewestAt
	}
	if part.Status == api.PacketRetentionStatusRecording && total.Status != api.PacketRetentionStatusUnavailable {
		total.Status = part.Status
	}
	if part.Error != nil || part.Status == api.PacketRetentionStatusUnavailable {
		total.Error = part.Error
		total.Status = api.PacketRetentionStatusUnavailable
	}
}
func (r *Recorder) Recent(ctx context.Context, environment string, q RecentQuery) (api.RecentPacketPage, error) {
	result := api.RecentPacketPage{Packets: []api.CapturePacket{}, Errors: map[string]string{}, Retention: r.Retention()[environment]}
	cursor, err := parsePacketCursor(q.Cursor)
	if err != nil {
		return result, err
	}
	parts, err := r.openParts(environment)
	if err != nil {
		return result, err
	}
	defer closeParts(parts)
	if len(parts) == 0 && q.Filter != "" {
		var empty bytes.Buffer
		if err = EmptyPacketCapture(&empty); err != nil {
			return result, err
		}
		command := exec.CommandContext(ctx, "tshark", "-n", "-r", "-", "-Y", q.Filter, "-T", "fields", "-e", "frame.number")
		command.Stdin = &empty
		if data, filterErr := command.CombinedOutput(); filterErr != nil {
			return result, fmt.Errorf("报文筛选失败：%s", strings.TrimSpace(string(data)))
		}
	}
	for index := len(parts) - 1; index >= 0; index-- {
		part := parts[index]
		if part.part.CaptureID != "" {
			continue
		}
		if !q.Start.IsZero() && part.part.End.Before(q.Start) || !q.End.IsZero() && part.part.Start.After(q.End) {
			continue
		}
		if len(result.Packets) > q.Limit && part.part.End.Before(result.Packets[0].Time) {
			break
		}
		filter, matches := q.display(part.part)
		if !matches {
			continue
		}
		if err = r.openPart(environment, &part); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return result, err
		}
		parts[index] = part
		args := []string{"-n", "-r", "-", "-T", "fields", "-E", "separator=\t", "-E", "quote=d", "-E", "occurrence=f"}
		if filter != "" {
			args = append(args, "-Y", filter)
		}
		for _, field := range []string{"frame.number", "frame.time_epoch", "frame.len", "frame.cap_len", "frame.interface_id", "eth.src", "eth.dst", "ip.src", "ip.dst", "ipv6.src", "ipv6.dst", "tcp.srcport", "tcp.dstport", "udp.srcport", "udp.dstport", "_ws.col.Protocol", "_ws.col.Info", "icmp.type", "icmpv6.type"} {
			args = append(args, "-e", field)
		}
		command := exec.CommandContext(ctx, "tshark", args...)
		input := completePartReader(ctx, part, nil)
		defer input.Close()
		command.Stdin = input
		stdout, pipeErr := command.StdoutPipe()
		if pipeErr != nil {
			return result, pipeErr
		}
		var stderr strings.Builder
		command.Stderr = &stderr
		if err = command.Start(); err != nil {
			return result, err
		}
		reader := csv.NewReader(stdout)
		reader.Comma = '\t'
		reader.FieldsPerRecord = 19
		for {
			fields, readErr := reader.Read()
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				command.Process.Kill()
				command.Wait()
				return result, readErr
			}
			number, parseErr := strconv.ParseInt(fields[0], 10, 64)
			if parseErr != nil {
				return result, parseErr
			}
			epoch, parseErr := strconv.ParseFloat(fields[1], 64)
			if parseErr != nil {
				return result, parseErr
			}
			ifaceIndex, _ := strconv.Atoi(fields[4])
			if ifaceIndex < 0 || ifaceIndex >= len(part.part.Interfaces) {
				continue
			}
			iface := part.part.Interfaces[ifaceIndex]
			id := packetIdentity(r.node, part.part.ID, number)
			packet := api.CapturePacket{Id: &id, NodeId: &r.node, AssetId: &iface.AssetId, InterfaceId: &iface.InterfaceId, NetworkId: &iface.NetworkId, Number: number, Time: time.Unix(0, int64(epoch*1e9)).UTC(), Length: port(fields[2]), CapturedLength: port(fields[3]), InterfaceIndex: ifaceIndex, Source: first(fields[7], fields[9], fields[5]), Destination: first(fields[8], fields[10], fields[6]), SourcePort: port(first(fields[11], fields[13])), DestinationPort: port(first(fields[12], fields[14])), Protocol: fields[15], Summary: fields[16], SourceMac: &fields[5], DestinationMac: &fields[6]}
			transport := ""
			switch {
			case fields[11] != "" || fields[12] != "":
				transport = "TCP"
			case fields[13] != "" || fields[14] != "":
				transport = "UDP"
			case fields[17] != "":
				transport = "ICMP"
			case fields[18] != "":
				transport = "ICMPv6"
			}
			if transport != "" {
				packet.TransportProtocol = &transport
			}
			if !packetBefore(packet, cursor) {
				continue
			}
			at, _ := slices.BinarySearchFunc(result.Packets, packet, packetOrder)
			result.Packets = slices.Insert(result.Packets, at, packet)
			if len(result.Packets) > q.Limit+1 {
				result.Packets = result.Packets[1:]
			}
		}
		err = command.Wait()
		input.Close()
		closeParts([]partRead{part})
		parts[index].reader, parts[index].file = nil, nil
		if err != nil {
			return result, fmt.Errorf("报文筛选失败：%s: %w", strings.TrimSpace(stderr.String()), err)
		}
	}
	if len(result.Packets) > q.Limit {
		result.Packets = result.Packets[1:]
		cursor := packetCursor(result.Packets[0])
		result.NextCursor = &cursor
	}
	return result, nil
}

// The active zstd frame has no final footer. Re-emit committed complete packets,
// so TShark always receives a complete pcapng stream rather than a truncated archive.
func completePartReader(ctx context.Context, part partRead, selected []bool) *io.PipeReader {
	reader, writer := io.Pipe()
	go func() {
		output, err := pcapgo.NewNgWriterInterface(writer, pcapInterface(part.part.Interfaces[0]), pcapgo.DefaultNgWriterOptions)
		if err != nil {
			writer.CloseWithError(err)
			return
		}
		for _, iface := range part.part.Interfaces[1:] {
			if _, err = output.AddInterface(pcapInterface(iface)); err != nil {
				writer.CloseWithError(err)
				return
			}
		}
		var statisticsErr error
		input, err := pcapgo.NewNgReader(part.reader, pcapgo.NgReaderOptions{WantMixedLinkType: true, StatisticsCallback: func(index int, statistics pcapgo.NgInterfaceStatistics) {
			statisticsErr = errors.Join(statisticsErr, output.WriteInterfaceStats(index, statistics))
		}})
		if err == nil {
			for number := int64(1); ctx.Err() == nil; number++ {
				var data []byte
				var info gopacket.CaptureInfo
				data, info, err = input.ZeroCopyReadPacketData()
				if err != nil || number > part.part.Packets {
					break
				}
				if selected != nil && !selected[number] {
					continue
				}
				if err = output.WritePacket(info, data); err != nil {
					break
				}
			}
		}
		if errors.Is(err, io.EOF) || !part.part.Sealed && errors.Is(err, io.ErrUnexpectedEOF) {
			err = nil
		}
		writer.CloseWithError(errors.Join(err, statisticsErr, ctx.Err(), output.Flush()))
	}()
	return reader
}
func (r *Recorder) RecentPacket(ctx context.Context, environment, id string) (api.CapturePacketDetail, error) {
	node, partID, number, err := ParsePacketIdentity(id)
	result := api.CapturePacketDetail{Layers: []api.PacketField{}}
	if err != nil {
		return result, err
	}
	if node != r.node {
		return result, os.ErrNotExist
	}
	parts, err := r.openParts(environment)
	if err != nil {
		return result, err
	}
	defer closeParts(parts)
	for index, part := range parts {
		if part.part.ID != partID {
			continue
		}
		if err = r.openPart(environment, &part); err != nil {
			return result, err
		}
		parts[index] = part
		err = readFrames(ctx, part.reader, !part.part.Sealed, func(packet api.CapturePacket, data []byte) bool {
			if packet.Number != number {
				return true
			}
			iface := part.part.Interfaces[packet.InterfaceIndex]
			packet.Id, packet.NodeId, packet.AssetId, packet.InterfaceId, packet.NetworkId = &id, &r.node, &iface.AssetId, &iface.InterfaceId, &iface.NetworkId
			result.Packet, result.Hex = packet, hex.EncodeToString(data)
			return false
		})
		if err != nil {
			return result, err
		}
		if result.Packet.Number == 0 {
			return result, os.ErrNotExist
		}
		part.reader.Reset(io.NewSectionReader(part.file, 0, part.part.Size))
		command := exec.CommandContext(ctx, "tshark", "-n", "-r", "-", "-c", strconv.FormatInt(number, 10), "-Y", "frame.number == "+strconv.FormatInt(number, 10), "-T", "pdml")
		input := completePartReader(ctx, part, nil)
		defer input.Close()
		command.Stdin = input
		data, decodeErr := command.Output()
		if decodeErr != nil {
			return result, decodeErr
		}
		var document struct {
			Packets []struct {
				Protocols []packetFieldXML `xml:"proto"`
			} `xml:"packet"`
		}
		if err = xml.Unmarshal(data, &document); err != nil {
			return result, err
		}
		if len(document.Packets) != 1 {
			return result, errors.New("报文解析结果为空")
		}
		for _, field := range document.Packets[0].Protocols {
			result.Layers = append(result.Layers, field.model())
		}
		return result, nil
	}
	return result, os.ErrNotExist
}

type packetFieldXML struct {
	Name     string           `xml:"name,attr"`
	Label    string           `xml:"showname,attr"`
	Show     string           `xml:"show,attr"`
	Offset   int              `xml:"pos,attr"`
	Length   int              `xml:"size,attr"`
	Children []packetFieldXML `xml:",any"`
}

func (field packetFieldXML) model() api.PacketField {
	result := api.PacketField{Name: field.Name, Label: first(field.Label, field.Name), Offset: field.Offset, Length: field.Length, Children: []api.PacketField{}}
	if field.Show != "" {
		result.Value = &field.Show
	}
	for _, child := range field.Children {
		result.Children = append(result.Children, child.model())
	}
	return result
}
func matchingFrames(ctx context.Context, part partRead, filter string) ([]bool, error) {
	command := exec.CommandContext(ctx, "tshark", "-n", "-r", "-", "-Y", filter, "-T", "fields", "-e", "frame.number")
	input := completePartReader(ctx, part, nil)
	defer input.Close()
	command.Stdin = input
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	command.Stderr = &stderr
	if err = command.Start(); err != nil {
		return nil, err
	}
	selected := make([]bool, part.part.Packets+1)
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		var number int
		number, err = strconv.Atoi(scanner.Text())
		if err != nil {
			break
		}
		selected[number] = true
	}
	if err = errors.Join(err, scanner.Err()); err != nil {
		command.Process.Kill()
	}
	if err = errors.Join(err, command.Wait()); err != nil {
		return nil, fmt.Errorf("报文筛选失败：%s: %w", strings.TrimSpace(stderr.String()), err)
	}
	return selected, nil
}

func (r *Recorder) Export(ctx context.Context, environment string, q RecentQuery, output io.Writer) error {
	parts, err := r.openParts(environment)
	if err != nil {
		return err
	}
	defer closeParts(parts)
	wrote := false
	for _, part := range parts {
		if !q.Start.IsZero() && part.part.End.Before(q.Start) || !q.End.IsZero() && part.part.Start.After(q.End) {
			continue
		}
		if part.part.CaptureID != "" {
			continue
		}
		filter, matches := q.display(part.part)
		if !matches {
			continue
		}
		if err = r.openPart(environment, &part); err != nil {
			return err
		}
		var selected []bool
		if filter != "" {
			selected, err = matchingFrames(ctx, part, filter)
			if err != nil {
				closeParts([]partRead{part})
				return err
			}
			if err = part.reader.Reset(io.NewSectionReader(part.file, 0, part.part.Size)); err != nil {
				closeParts([]partRead{part})
				return err
			}
		}
		input := completePartReader(ctx, part, selected)
		wrote = true
		_, err = io.Copy(output, input)
		input.Close()
		closeParts([]partRead{part})
		if err != nil {
			return err
		}
	}
	if !wrote {
		return EmptyPacketCapture(output)
	}
	return nil
}
