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
	addresses               map[string]api.CaptureInterface
	macs                    map[string]api.CaptureInterface
	flows                   map[string]api.CaptureFlow
	packets, bytes, omitted int64
}
type secondBucket struct {
	second int64
	bytes  int64
}

// Sample is either one captured frame or a statistically weighted sFlow sample.
type Sample struct {
	At                                                       time.Time
	Source, Destination, SourceMAC, DestinationMAC, Protocol string
	SourcePort, DestinationPort                              int
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
	a := &aggregate{started: time.Now(), window: map[string][10]secondBucket{}, node: node, selected: map[string]bool{}, addresses: map[string]api.CaptureInterface{}, macs: map[string]api.CaptureInterface{}, flows: map[string]api.CaptureFlow{}}
	for _, id := range request.Settings.AssetIds {
		a.selected[id] = true
	}
	for _, iface := range request.Interfaces {
		a.macs[strings.ToLower(iface.Mac)] = iface
		address := iface.Address
		if prefix, err := netip.ParsePrefix(address); err == nil {
			address = prefix.Addr().String()
		}
		if address != "" {
			if previous, exists := a.addresses[address]; exists && previous.AssetId != iface.AssetId {
				a.addresses[address] = api.CaptureInterface{}
			} else {
				a.addresses[address] = iface
			}
		}
	}
	return a
}

func (a *aggregate) add(line string) error {
	f := strings.Split(line, "\t")
	if len(f) != 13 {
		return fmt.Errorf("抓包字段数量为 %d，预期 13", len(f))
	}
	epoch, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return err
	}
	size, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil || size < 0 {
		return fmt.Errorf("无效帧大小 %q", f[1])
	}
	a.addSample(Sample{At: time.Unix(0, int64(epoch*1e9)).UTC(), Bytes: size, Packets: 1,
		Source: first(f[4], f[6], f[2]), Destination: first(f[5], f[7], f[3]), SourceMAC: f[2], DestinationMAC: f[3],
		SourcePort: port(first(f[8], f[10])), DestinationPort: port(first(f[9], f[11])), Protocol: f[12]})
	return nil
}

func (a *aggregate) addSample(sample Sample) {
	a.packets += sample.Packets
	a.bytes += sample.Bytes
	source, destination := sample.Source, sample.Destination
	src, srcKnown := a.endpoint(source, sample.SourceMAC)
	dst, dstKnown := a.endpoint(destination, sample.DestinationMAC)
	// A cross-node packet contributes once: selected sender first, otherwise selected receiver.
	owner := ""
	if srcKnown && a.selected[src.AssetId] {
		owner = src.NodeId
	} else if dstKnown && a.selected[dst.AssetId] {
		owner = dst.NodeId
	}
	if owner != a.node {
		return
	}
	sp, dp := sample.SourcePort, sample.DestinationPort
	key := strings.Join([]string{source, destination, sample.SourceMAC, sample.DestinationMAC, sample.Protocol, strconv.Itoa(sp), strconv.Itoa(dp)}, "\x00")
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
func (a *aggregate) endpoint(address, mac string) (api.CaptureInterface, bool) {
	if iface, exists := a.macs[strings.ToLower(mac)]; exists {
		return iface, true
	}
	iface, exists := a.addresses[address]
	return iface, exists && iface.AssetId != ""
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
