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

// The packet decoder is tshark. This layer only correlates identities and aggregates its field stream.
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
	a.packets++
	a.bytes += size
	source, destination := first(f[4], f[6], f[2]), first(f[5], f[7], f[3])
	src, srcKnown := a.endpoint(source, f[2])
	dst, dstKnown := a.endpoint(destination, f[3])
	// A cross-node packet contributes once: selected sender first, otherwise selected receiver.
	owner := ""
	if srcKnown && a.selected[src.AssetId] {
		owner = src.NodeId
	} else if dstKnown && a.selected[dst.AssetId] {
		owner = dst.NodeId
	}
	if owner != a.node {
		return nil
	}
	sp, dp := port(first(f[8], f[10])), port(first(f[9], f[11]))
	key := strings.Join([]string{source, destination, f[12], strconv.Itoa(sp), strconv.Itoa(dp)}, "\x00")
	flow, exists := a.flows[key]
	if !exists && len(a.flows) >= flowLimit {
		a.omitted++
		return nil
	}
	at := time.Unix(0, int64(epoch*1e9)).UTC()
	if !exists {
		flow = api.CaptureFlow{Source: source, Destination: destination, Protocol: f[12], SourcePort: sp, DestinationPort: dp, FirstSeen: at}
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
	flow.Bytes += size
	flow.Packets++
	flow.LastSeen = at
	a.flows[key] = flow
	buckets := a.window[key]
	second := at.Unix()
	index := (second%10 + 10) % 10
	if buckets[index].second != second {
		buckets[index] = secondBucket{second: second}
	}
	buckets[index].bytes += size
	a.window[key] = buckets
	return nil
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
	slices.SortFunc(items, func(a, b api.CaptureFlow) int {
		return cmp.Or(cmp.Compare(b.Bytes, a.Bytes), strings.Compare(a.Source, b.Source), strings.Compare(a.Destination, b.Destination), strings.Compare(a.Protocol, b.Protocol), cmp.Compare(a.SourcePort, b.SourcePort), cmp.Compare(a.DestinationPort, b.DestinationPort))
	})
	return items
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
