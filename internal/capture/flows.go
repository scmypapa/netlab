package capture

import (
	"cmp"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"netlab.local/core/api"
)

const flowLimit = 4000
const SamplingRate = 1
const ObservationWindow = 60

// Capture and IPFIX decoders share identity correlation and conversation aggregation.
type aggregate struct {
	started                 time.Time
	node                    string
	selected                map[string]bool
	ports                   map[string]api.CaptureInterface
	capturePorts            []string
	addresses               map[endpointKey]api.CaptureInterface
	macs                    map[endpointKey]api.CaptureInterface
	flows                   map[flowKey]conversation
	packets, bytes, omitted int64
}
type secondBucket struct {
	second int64
	bytes  int64
}
type flowKey struct {
	sourceNetwork, destinationNetwork, sourceAsset, destinationAsset string
	packetKey
}
type packetKey struct {
	source, destination, sourceMAC, destinationMAC, protocol string
	sourcePort, destinationPort                              int
}
type conversation struct {
	flow   api.CaptureFlow
	window [10]secondBucket
}
type endpointKey struct{ network, value string }

const (
	sourcePortSide uint8 = iota + 1
	destinationPortSide
)

// Sample is one captured frame or an exported IPFIX flow record.
type Sample struct {
	At, LastSeen                                             time.Time
	Source, Destination, SourceMAC, DestinationMAC, Protocol string
	AppProtocol                                              string
	CaptureIndex                                             *int
	SourcePort, DestinationPort                              int
	PortName                                                 string
	Bytes, Packets, Samples                                  int64
	portSide                                                 uint8
}

func (sample Sample) key() packetKey {
	return packetKey{sample.Source, sample.Destination, sample.SourceMAC, sample.DestinationMAC, sample.Protocol, sample.SourcePort, sample.DestinationPort}
}

func Summarize(node string, interfaces []api.CaptureInterface, samples []Sample, now time.Time) ([]api.CaptureFlow, int64) {
	request := api.NodeCaptureRequest{Interfaces: interfaces}
	for _, iface := range interfaces {
		request.Settings.AssetIds = append(request.Settings.AssetIds, iface.AssetId)
	}
	a := newAggregate(node, request)
	for _, sample := range samples {
		if sample.At.Before(a.started) {
			a.started = sample.At
		}
		a.addSample(sample)
	}
	return a.snapshotAt(now), a.omitted
}

func newAggregate(node string, request api.NodeCaptureRequest) *aggregate {
	a := &aggregate{started: time.Now(), node: node, selected: map[string]bool{}, ports: map[string]api.CaptureInterface{}, addresses: map[endpointKey]api.CaptureInterface{}, macs: map[endpointKey]api.CaptureInterface{}, flows: map[flowKey]conversation{}}
	for _, id := range request.Settings.AssetIds {
		a.selected[id] = true
	}
	for _, iface := range request.Interfaces {
		a.ports[iface.PortName] = iface
		address := iface.Address
		if prefix, err := netip.ParsePrefix(address); err == nil {
			address = prefix.Addr().String()
		}
		for _, entry := range []struct {
			index map[endpointKey]api.CaptureInterface
			value string
		}{{a.macs, strings.ToLower(iface.Mac)}, {a.addresses, address}} {
			if entry.value == "" {
				continue
			}
			for _, scope := range []string{iface.NetworkId, ""} {
				key := endpointKey{scope, entry.value}
				if previous, exists := entry.index[key]; exists && (previous.InterfaceId != iface.InterfaceId || previous.AssetId != iface.AssetId) {
					entry.index[key] = api.CaptureInterface{}
				} else {
					entry.index[key] = iface
				}
			}
		}
	}
	return a
}

func (a *aggregate) add(line string) error {
	f := strings.Split(line, "\t")
	if len(f) != 14 {
		return fmt.Errorf("抓包字段数量为 %d，预期 14", len(f))
	}
	epoch, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return err
	}
	size, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil || size < 0 {
		return fmt.Errorf("无效帧大小 %q", f[1])
	}
	index, err := strconv.Atoi(f[13])
	if err != nil || index < 0 || index >= len(a.capturePorts) {
		return fmt.Errorf("无效抓包接口序号 %q", f[13])
	}
	protocol, application := f[12], ""
	if f[8] != "" || f[9] != "" {
		protocol = "TCP"
	} else if f[10] != "" || f[11] != "" {
		protocol = "UDP"
	}
	if protocol != f[12] {
		application = f[12]
	}
	a.addSample(Sample{At: time.Unix(0, int64(epoch*1e9)).UTC(), Bytes: size, Packets: 1, Samples: 1, PortName: a.capturePorts[index],
		Source: first(f[4], f[6], f[2]), Destination: first(f[5], f[7], f[3]), SourceMAC: f[2], DestinationMAC: f[3],
		SourcePort: port(first(f[8], f[10])), DestinationPort: port(first(f[9], f[11])), Protocol: protocol, AppProtocol: application, CaptureIndex: &index})
	return nil
}

func (a *aggregate) addSample(sample Sample) {
	source, destination := sample.Source, sample.Destination
	network := a.ports[sample.PortName].NetworkId
	src, srcKnown := a.endpoint(source, sample.SourceMAC, network)
	dst, dstKnown := a.endpoint(destination, sample.DestinationMAC, network)
	// The sampled attachment identifies the local endpoint even for external L2 broadcasts.
	if local := a.ports[sample.PortName]; local.AssetId != "" {
		if !srcKnown && sample.portSide == sourcePortSide {
			src, srcKnown = local, true
		}
		if !dstKnown && sample.portSide == destinationPortSide {
			dst, dstKnown = local, true
		}
	}
	// A cross-node packet contributes once: selected sender first, otherwise selected receiver.
	owner := api.CaptureInterface{}
	if srcKnown && a.selected[src.AssetId] {
		owner = src
	} else if dstKnown && a.selected[dst.AssetId] {
		owner = dst
	}
	if owner.NodeId != a.node || sample.PortName != "" && owner.PortName != sample.PortName {
		return
	}
	a.packets += sample.Packets
	a.bytes += sample.Bytes
	sp, dp := sample.SourcePort, sample.DestinationPort
	key := flowKey{src.NetworkId, dst.NetworkId, src.AssetId, dst.AssetId, sample.key()}
	entry, exists := a.flows[key]
	if !exists && len(a.flows) >= flowLimit {
		a.omitted += sample.Samples
		return
	}
	at := sample.At
	flow := &entry.flow
	if !exists {
		*flow = api.CaptureFlow{Source: source, Destination: destination, Protocol: sample.Protocol, SourcePort: sp, DestinationPort: dp, FirstSeen: at}
		node := a.node
		flow.NodeId, flow.InterfaceIndex = &node, sample.CaptureIndex
		if srcKnown {
			id := src.AssetId
			flow.SourceAssetId = &id
			if src.AssetName != "" {
				name := src.AssetName
				flow.SourceAssetName = &name
			}
		}
		if dstKnown {
			id := dst.AssetId
			flow.DestinationAssetId = &id
			if dst.AssetName != "" {
				name := dst.AssetName
				flow.DestinationAssetName = &name
			}
		}
	}
	flow.Bytes += sample.Bytes
	if sample.AppProtocol != "" {
		application := sample.AppProtocol
		flow.AppProtocol = &application
	}
	flow.Packets += sample.Packets
	if at.Before(flow.FirstSeen) {
		flow.FirstSeen = at
	}
	last := sample.LastSeen
	if last.IsZero() {
		last = at
	}
	if last.After(flow.LastSeen) {
		flow.LastSeen = last
	}
	second := last.Unix()
	index := (second%10 + 10) % 10
	if entry.window[index].second < second {
		entry.window[index] = secondBucket{second: second}
	}
	if entry.window[index].second == second {
		entry.window[index].bytes += sample.Bytes
	}
	a.flows[key] = entry
}

func (a *aggregate) snapshot() []api.CaptureFlow {
	return a.snapshotAt(time.Now())
}
func (a *aggregate) snapshotAt(now time.Time) []api.CaptureFlow {
	items := make([]api.CaptureFlow, 0, len(a.flows))
	start := time.Unix(now.Unix()-9, 0)
	if a.started.After(start) {
		start = a.started
	}
	for _, entry := range a.flows {
		flow := entry.flow
		var bytes int64
		for _, bucket := range entry.window {
			if bucket.second >= start.Unix() && bucket.second <= now.Unix() {
				bytes += bucket.bytes
			}
		}
		if seconds := now.Sub(start).Seconds(); seconds > 0 {
			flow.BytesPerSecond = float64(bytes) / seconds
		}
		items = append(items, flow)
	}
	SortFlows(items)
	return items
}

func SortFlows(items []api.CaptureFlow) {
	asset := func(id *string) string {
		if id == nil {
			return ""
		}
		return *id
	}
	slices.SortFunc(items, func(a, b api.CaptureFlow) int {
		return cmp.Or(cmp.Compare(b.Bytes, a.Bytes), strings.Compare(a.Source, b.Source), strings.Compare(a.Destination, b.Destination), strings.Compare(a.Protocol, b.Protocol), cmp.Compare(a.SourcePort, b.SourcePort), cmp.Compare(a.DestinationPort, b.DestinationPort), strings.Compare(asset(a.SourceAssetId), asset(b.SourceAssetId)), strings.Compare(asset(a.DestinationAssetId), asset(b.DestinationAssetId)))
	})
}
func (a *aggregate) endpoint(address, mac, network string) (api.CaptureInterface, bool) {
	for _, scope := range []string{network, ""} {
		for _, entry := range []struct {
			index map[endpointKey]api.CaptureInterface
			value string
		}{{a.macs, strings.ToLower(mac)}, {a.addresses, address}} {
			if iface := entry.index[endpointKey{scope, entry.value}]; iface.AssetId != "" {
				return iface, true
			}
		}
	}
	return api.CaptureInterface{}, false
}
func first(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
func port(value string) int { result, _ := strconv.Atoi(value); return result }
