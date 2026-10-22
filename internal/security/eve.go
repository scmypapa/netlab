package security

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"netlab.local/core/api"
)

type attachment struct {
	Device string `json:"device"`
	api.SecurityInterface
}

type eveRecord struct {
	Timestamp       string `json:"timestamp"`
	Type            string `json:"event_type"`
	Device          string `json:"in_iface"`
	Source          string `json:"src_ip"`
	SourcePort      int    `json:"src_port"`
	Destination     string `json:"dest_ip"`
	DestinationPort int    `json:"dest_port"`
	Protocol        string `json:"proto"`
	AppProtocol     string `json:"app_proto"`
	Ethernet        struct {
		Source      string `json:"src_mac"`
		Destination string `json:"dest_mac"`
	} `json:"ethernet"`
	Alert *struct {
		Generator   int64  `json:"gid"`
		SignatureID int64  `json:"signature_id"`
		Revision    int    `json:"rev"`
		Severity    int    `json:"severity"`
		Category    string `json:"category"`
		Signature   string `json:"signature"`
	} `json:"alert"`
	Stats struct {
		Capture struct {
			Packets int64 `json:"kernel_packets"`
			Drops   int64 `json:"kernel_drops"`
		} `json:"capture"`
	} `json:"stats"`
}

type peerAddress struct {
	environment, network, address string
}

type eveDecoder struct {
	node    string
	devices map[string]attachment
	peers   []api.SecurityInterface
	byIP    map[peerAddress]int
	byMAC   map[peerAddress]int
}

func newEVEDecoder(node string, devices []attachment, peers []api.SecurityInterface) eveDecoder {
	d := eveDecoder{node: node, devices: make(map[string]attachment, len(devices)), peers: peers,
		byIP: make(map[peerAddress]int, len(peers)), byMAC: make(map[peerAddress]int, len(peers))}
	for _, device := range devices {
		d.devices[device.Device] = device
	}
	// Reverse insertion preserves the first matching interface in the snapshot.
	for i := len(peers) - 1; i >= 0; i-- {
		peer := peers[i]
		ip := peer.Interface.Address
		if prefix, err := netip.ParsePrefix(ip); err == nil {
			ip = prefix.Addr().String()
		}
		d.byIP[peerAddress{peer.EnvironmentId, peer.Interface.NetworkId, ip}] = i
		d.byMAC[peerAddress{peer.EnvironmentId, peer.Interface.NetworkId, strings.ToLower(peer.Interface.Mac)}] = i
	}
	return d
}

func (d eveDecoder) resolve(origin attachment, address, mac string) *api.CaptureInterface {
	ipIndex, ipOK := d.byIP[peerAddress{origin.EnvironmentId, origin.Interface.NetworkId, address}]
	macIndex, macOK := d.byMAC[peerAddress{origin.EnvironmentId, origin.Interface.NetworkId, strings.ToLower(mac)}]
	ipOK, macOK = ipOK && address != "", macOK && mac != ""
	if macOK && (!ipOK || macIndex < ipIndex) {
		return &d.peers[macIndex].Interface
	}
	if ipOK {
		return &d.peers[ipIndex].Interface
	}
	return nil
}

func (d eveDecoder) decode(raw []byte) (eveRecord, *api.SecurityAlert, error) {
	var event eveRecord
	if err := json.Unmarshal(raw, &event); err != nil {
		return event, nil, err
	}
	if event.Type != "alert" {
		return event, nil, nil
	}
	at, err := time.Parse("2006-01-02T15:04:05.999999999-0700", event.Timestamp)
	if err != nil {
		return event, nil, err
	}
	if event.Alert == nil || event.Alert.Severity < 1 || event.Alert.Severity > 255 {
		return event, nil, fmt.Errorf("无效 Suricata 告警")
	}
	origin, ok := d.devices[event.Device]
	if !ok {
		return event, nil, fmt.Errorf("Suricata 采集接口不在本轮配置中：%s", event.Device)
	}
	source := d.resolve(origin, event.Source, event.Ethernet.Source)
	destination := d.resolve(origin, event.Destination, event.Ethernet.Destination)
	// Both endpoint taps see the same conversation. Its source owns detection;
	// external ingress belongs to the managed destination, including across nodes.
	owner := source
	if owner == nil {
		owner = destination
	}
	if owner != nil && (owner.NodeId != d.node || owner.InterfaceId != origin.Interface.InterfaceId) {
		return event, nil, nil
	}
	alert := &api.SecurityAlert{
		EnvironmentId: origin.EnvironmentId, NodeId: d.node, NetworkId: origin.Interface.NetworkId, InterfaceId: origin.Interface.InterfaceId,
		Source: event.Source, SourcePort: event.SourcePort, Destination: event.Destination, DestinationPort: event.DestinationPort,
		Protocol: event.Protocol, AppProtocol: event.AppProtocol,
		GeneratorId: event.Alert.Generator, SignatureId: event.Alert.SignatureID, Revision: event.Alert.Revision,
		Severity: event.Alert.Severity, Category: event.Alert.Category, Signature: event.Alert.Signature,
		Count: 1, FirstSeen: at, LastSeen: at,
	}
	if source != nil {
		alert.SourceAssetId, alert.SourceAssetName = source.AssetId, source.AssetName
	}
	if destination != nil {
		alert.DestinationAssetId, alert.DestinationAssetName = destination.AssetId, destination.AssetName
	}
	return event, alert, nil
}
