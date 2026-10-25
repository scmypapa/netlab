package capture

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/gopacket/gopacket/layers"
	"github.com/netsampler/goflow2/v2/decoders/netflow"
	"netlab.local/core/api"
	"netlab.local/core/internal/network"
)

const sampleBudget = 65536

type samplePort struct {
	name            string
	ingress, egress uint32
}

func (port samplePort) resolve(interfaces map[uint32]string) (string, uint8) {
	if port.name != "" {
		return port.name, 0
	}
	if name := interfaces[port.ingress]; name != "" {
		return name, sourcePortSide
	}
	return interfaces[port.egress], destinationPortSide
}

type sampleKey struct {
	packetKey
	port   samplePort
	second int64
}
type sampled struct {
	Sample
	key sampleKey
}
type sampleDrop struct {
	second int64
	ports  map[samplePort]int64
}
type Collector struct {
	ovs        *network.OVS
	node       string
	conn       *net.UDPConn
	workers    sync.WaitGroup
	mu         sync.Mutex
	interfaces map[uint32]string
	samples    []sampled
	index      map[samplePort]map[sampleKey]int
	next       int
	omitted    [ObservationWindow]sampleDrop
	err        error
	templates  netflow.NetFlowTemplateSystem
}

func NewCollector(ctx context.Context, ovs *network.OVS, node string) (*Collector, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	interfaces, err := ovs.FlowInterfaces(ctx)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if err = ovs.SetFlowExport(ctx, node, conn.LocalAddr().String()); err != nil {
		conn.Close()
		return nil, err
	}
	s := &Collector{ovs: ovs, node: node, conn: conn, interfaces: interfaces, index: map[samplePort]map[sampleKey]int{}, templates: netflow.CreateTemplateSystem()}
	s.workers.Add(1)
	go func() { defer s.workers.Done(); s.receive() }()
	return s, nil
}

func (s *Collector) Close() error {
	s.conn.Close()
	s.workers.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.ovs.SetFlowExport(ctx, s.node, "")
}

func (s *Collector) receive() {
	data := make([]byte, 65535)
	for {
		n, _, err := s.conn.ReadFromUDP(data)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				s.mu.Lock()
				s.err = err
				s.mu.Unlock()
			}
			return
		}
		if err := s.ingest(data[:n], time.Now().UTC()); err != nil {
			s.mu.Lock()
			s.err = err
			s.mu.Unlock()
		}
	}
}

func (s *Collector) ingest(data []byte, now time.Time) error {
	if len(data) < 2 || binary.BigEndian.Uint16(data) != 10 {
		return fmt.Errorf("expected IPFIX version 10")
	}
	var datagram netflow.IPFIXPacket
	if err := netflow.DecodeMessageVersion(bytes.NewBuffer(data), s.templates, nil, &datagram); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = nil
	for _, set := range datagram.FlowSets {
		flow, ok := set.(netflow.DataFlowSet)
		if !ok {
			continue
		}
		for _, record := range flow.Records {
			sample, port := decodeFlow(record, time.Unix(int64(datagram.ExportTime), 0).UTC())
			if sample.Packets == 0 {
				continue
			}
			port.name, sample.portSide = port.resolve(s.interfaces)
			sample.PortName = port.name
			s.add(sample, port)
		}
	}
	return nil
}

// Keep one record per flow-second, so a busy flow cannot evict an entire node's observations by packet count.
func (s *Collector) add(sample Sample, port samplePort) {
	key := sampleKey{sample.key(), port, sample.At.Unix()}
	if index, exists := s.index[port][key]; exists {
		entry := &s.samples[index]
		entry.Bytes += sample.Bytes
		entry.Packets += sample.Packets
		entry.Samples += sample.Samples
		if sample.At.Before(entry.At) {
			entry.At = sample.At
		}
		if sample.LastSeen.After(entry.LastSeen) {
			entry.LastSeen = sample.LastSeen
		}
		return
	}
	index := len(s.samples)
	entry := sampled{Sample: sample, key: key}
	if index < sampleBudget {
		s.samples = append(s.samples, entry)
	} else {
		index = s.next
		previous := s.samples[index]
		delete(s.index[previous.key.port], previous.key)
		if len(s.index[previous.key.port]) == 0 {
			delete(s.index, previous.key.port)
		}
		if sample.At.Sub(previous.LastSeen) < ObservationWindow*time.Second {
			second := previous.At.Unix()
			drop := &s.omitted[second%ObservationWindow]
			if drop.second != second {
				*drop = sampleDrop{second: second, ports: map[samplePort]int64{}}
			}
			attachment := previous.key.port
			attachment.name = previous.PortName
			drop.ports[attachment] += previous.Samples
		}
		s.samples[index] = entry
		s.next = (index + 1) % sampleBudget
	}
	if s.index[port] == nil {
		s.index[port] = map[sampleKey]int{}
	}
	s.index[port][key] = index
}

func decodeFlow(record netflow.DataRecord, exported time.Time) (Sample, samplePort) {
	sample := Sample{At: exported, LastSeen: exported, Samples: 1}
	var ingress, egress uint32
	var direction uint64
	for _, field := range record.Values {
		if field.PenProvided {
			continue
		}
		value := field.Value.([]byte)
		number := func() uint64 {
			var result uint64
			for _, b := range value {
				result = result<<8 | uint64(b)
			}
			return result
		}
		switch field.Type {
		case netflow.IPFIX_FIELD_sourceMacAddress:
			sample.SourceMAC = net.HardwareAddr(value).String()
		case netflow.IPFIX_FIELD_destinationMacAddress:
			sample.DestinationMAC = net.HardwareAddr(value).String()
		case netflow.IPFIX_FIELD_sourceIPv4Address, netflow.IPFIX_FIELD_sourceIPv6Address:
			sample.Source = net.IP(value).String()
		case netflow.IPFIX_FIELD_destinationIPv4Address, netflow.IPFIX_FIELD_destinationIPv6Address:
			sample.Destination = net.IP(value).String()
		case netflow.IPFIX_FIELD_protocolIdentifier:
			sample.Protocol = layers.IPProtocol(number()).String()
		case netflow.IPFIX_FIELD_ethernetType:
			if sample.Protocol == "" {
				sample.Protocol = layers.EthernetType(number()).String()
			}
		case netflow.IPFIX_FIELD_sourceTransportPort:
			sample.SourcePort = int(number())
		case netflow.IPFIX_FIELD_destinationTransportPort:
			sample.DestinationPort = int(number())
		case netflow.IPFIX_FIELD_ingressInterface:
			ingress = uint32(number())
		case netflow.IPFIX_FIELD_egressInterface:
			egress = uint32(number())
		case netflow.IPFIX_FIELD_flowDirection:
			direction = number()
		case netflow.IPFIX_FIELD_packetDeltaCount:
			sample.Packets = int64(number())
		case netflow.IPFIX_FIELD_layer2OctetDeltaCount:
			sample.Bytes = int64(number())
		case netflow.IPFIX_FIELD_flowStartDeltaMicroseconds:
			sample.At = exported.Add(-time.Duration(number()) * time.Microsecond)
		case netflow.IPFIX_FIELD_flowEndDeltaMicroseconds:
			sample.LastSeen = exported.Add(-time.Duration(number()) * time.Microsecond)
		}
	}
	sample.Source, sample.Destination = first(sample.Source, sample.SourceMAC), first(sample.Destination, sample.DestinationMAC)
	if direction == 1 {
		return sample, samplePort{egress: egress}
	}
	return sample, samplePort{ingress: ingress}
}

func (s *Collector) Snapshot(ctx context.Context, interfaces []api.CaptureInterface, now time.Time) (api.TrafficObservation, error) {
	ports, err := s.ovs.FlowInterfaces(ctx)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return api.TrafficObservation{}, err
	}
	return s.snapshot(interfaces, ports, now)
}

func (s *Collector) snapshot(interfaces []api.CaptureInterface, resolved map[uint32]string, now time.Time) (api.TrafficObservation, error) {
	ports := map[string]bool{}
	for _, iface := range interfaces {
		if iface.NodeId == s.node {
			ports[iface.PortName] = true
		}
	}
	s.mu.Lock()
	s.interfaces = resolved
	if s.err != nil {
		err := s.err
		s.mu.Unlock()
		return api.TrafficObservation{}, err
	}
	var samples []Sample
	var omitted int64
	for _, drop := range s.omitted {
		if drop.second > now.Unix()-ObservationWindow {
			for port, count := range drop.ports {
				name, _ := port.resolve(resolved)
				if ports[name] {
					omitted += count
				}
			}
		}
	}
	for port, entries := range s.index {
		if port.name != "" && !ports[port.name] {
			continue
		}
		// Only unresolved attachment buckets need late binding; named buckets
		// let one environment avoid scanning every other environment's flows.
		for _, index := range entries {
			observed := &s.samples[index]
			if observed.PortName == "" {
				observed.PortName, observed.portSide = observed.key.port.resolve(resolved)
			}
			if ports[observed.PortName] && now.Sub(observed.LastSeen) <= ObservationWindow*time.Second {
				samples = append(samples, observed.Sample)
			}
		}
	}
	s.mu.Unlock()
	flows, excluded := Summarize(s.node, interfaces, samples, now)
	return api.TrafficObservation{Flows: flows, Errors: map[string]string{}, ObservedAt: now, SamplingRate: SamplingRate, WindowSeconds: ObservationWindow, OmittedSamples: omitted + excluded}, nil
}
