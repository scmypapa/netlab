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
const SamplingRate = 512
const ObservationWindow = 60

// Capture and sFlow decoders share identity correlation and conversation aggregation.
type aggregate struct {
	started                 time.Time
	window                  map[string][10]secondBucket
	node                    string
	selected                map[string]bool
	ports                   map[string]api.CaptureInterface
	capturePorts            []string
	addresses               map[endpointKey]api.CaptureInterface
	macs                    map[endpointKey]api.CaptureInterface
	flows                   map[string]api.CaptureFlow
	packets, bytes, omitted int64
}
type secondBucket struct {
	second int64
	bytes  int64
}
type endpointKey struct{ network, value string }

// Sample is either one captured frame or a statistically weighted sFlow sample.
type Sample struct {
	At                                                       time.Time
	Source, Destination, SourceMAC, DestinationMAC, Protocol string
	SourcePort, DestinationPort                              int
	PortName                                                 string
	Bytes, Packets                                           int64
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
	a := &aggregate{started: time.Now(), window: map[string][10]secondBucket{}, node: node, selected: map[string]bool{}, ports: map[string]api.CaptureInterface{}, addresses: map[endpointKey]api.CaptureInterface{}, macs: map[endpointKey]api.CaptureInterface{}, flows: map[string]api.CaptureFlow{}}
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
	a.addSample(Sample{At: time.Unix(0, int64(epoch*1e9)).UTC(), Bytes: size, Packets: 1, PortName: a.capturePorts[index],
		Source: first(f[4], f[6], f[2]), Destination: first(f[5], f[7], f[3]), SourceMAC: f[2], DestinationMAC: f[3],
		SourcePort: port(first(f[8], f[10])), DestinationPort: port(first(f[9], f[11])), Protocol: f[12]})
	return nil
}

func (a *aggregate) addSample(sample Sample) {
	source, destination := sample.Source, sample.Destination
	network := a.ports[sample.PortName].NetworkId
	src, srcKnown := a.endpoint(source, sample.SourceMAC, network)
	dst, dstKnown := a.endpoint(destination, sample.DestinationMAC, network)
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
	key := strings.Join([]string{src.NetworkId, dst.NetworkId, src.AssetId, dst.AssetId, source, destination, sample.SourceMAC, sample.DestinationMAC, sample.Protocol, strconv.Itoa(sp), strconv.Itoa(dp)}, "\x00")
	flow, exists := a.flows[key]
	if !exists && len(a.flows) >= flowLimit {
		a.omitted++
		return
	}
	at := sample.At
	if !exists {
		flow = api.CaptureFlow{Source: source, Destination: destination, Protocol: sample.Protocol, SourcePort: sp, DestinationPort: dp, FirstSeen: at}
		if srcKnown {
			flow.SourceAssetId = &src.AssetId
			if src.AssetName != "" {
				flow.SourceAssetName = &src.AssetName
			}
		}
		if dstKnown {
			flow.DestinationAssetId = &dst.AssetId
			if dst.AssetName != "" {
				flow.DestinationAssetName = &dst.AssetName
			}
		}
	}
	flow.Bytes += sample.Bytes
	flow.Packets += sample.Packets
	if at.Before(flow.FirstSeen) {
		flow.FirstSeen = at
	}
	if at.After(flow.LastSeen) {
		flow.LastSeen = at
	}
	a.flows[key] = flow
	buckets := a.window[key]
	second := at.Unix()
	index := (second%10 + 10) % 10
	if buckets[index].second < second {
		buckets[index] = secondBucket{second: second}
	}
	if buckets[index].second == second {
		buckets[index].bytes += sample.Bytes
	}
	a.window[key] = buckets
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
	for key, flow := range a.flows {
		var bytes int64
		for _, bucket := range a.window[key] {
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
