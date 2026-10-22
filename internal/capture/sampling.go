package capture

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"netlab.local/core/api"
	"netlab.local/core/internal/network"
)

const sampleBudget = 65536

type samplePort struct {
	name            string
	ingress, egress uint32
}

func (port samplePort) resolve(interfaces map[uint32]string) string {
	if port.name != "" {
		return port.name
	}
	if name := interfaces[port.ingress]; name != "" {
		return name
	}
	return interfaces[port.egress]
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
type Sampler struct {
	ovs        *network.OVS
	node       string
	conn       *net.UDPConn
	workers    sync.WaitGroup
	mu         sync.Mutex
	interfaces map[uint32]string
	samples    []sampled
	index      map[sampleKey]int
	next       int
	omitted    [ObservationWindow]sampleDrop
	err        error
}

func NewSampler(ctx context.Context, ovs *network.OVS, node string) (*Sampler, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	interfaces, err := ovs.SamplingInterfaces(ctx)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if err = ovs.SetSampling(ctx, node, conn.LocalAddr().String(), SamplingRate); err != nil {
		conn.Close()
		return nil, err
	}
	s := &Sampler{ovs: ovs, node: node, conn: conn, interfaces: interfaces, index: map[sampleKey]int{}}
	s.workers.Add(1)
	go func() { defer s.workers.Done(); s.receive() }()
	return s, nil
}

func (s *Sampler) Close() error {
	s.conn.Close()
	s.workers.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.ovs.SetSampling(ctx, s.node, "", SamplingRate)
}

func (s *Sampler) receive() {
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

func (s *Sampler) ingest(data []byte, now time.Time) error {
	packet := gopacket.NewPacket(data, layers.LayerTypeSFlow, gopacket.Default)
	if failure := packet.ErrorLayer(); failure != nil {
		return failure.Error()
	}
	datagram, ok := packet.Layer(layers.LayerTypeSFlow).(*layers.SFlowDatagram)
	if !ok {
		return errors.New("invalid sFlow datagram")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = nil
	for _, flow := range datagram.FlowSamples {
		port := samplePort{}
		if flow.InputInterfaceFormat == 0 {
			port.ingress = flow.InputInterface
		}
		if flow.OutputInterfaceFormat == 0 {
			port.egress = flow.OutputInterface
		}
		if port.ingress == 0 && port.egress == 0 || flow.SamplingRate == 0 {
			continue
		}
		for _, record := range flow.Records {
			header, ok := record.(layers.SFlowRawPacketFlowRecord)
			if !ok {
				continue
			}
			sample := decodeSample(header, flow.SamplingRate, now)
			if sample.Source == "" || sample.Destination == "" {
				continue
			}
			port.name = port.resolve(s.interfaces)
			sample.PortName = port.name
			s.add(sample, port)
			break
		}
	}
	return nil
}

// Keep one record per flow-second, so a busy flow cannot evict an entire node's observations by packet count.
func (s *Sampler) add(sample Sample, port samplePort) {
	key := sampleKey{sample.key(), port, sample.At.Unix()}
	if index, exists := s.index[key]; exists {
		entry := &s.samples[index]
		entry.Bytes += sample.Bytes
		entry.Packets += sample.Packets
		entry.Samples += sample.Samples
		if sample.At.Before(entry.At) {
			entry.At = sample.At
		}
		if sample.At.After(entry.LastSeen) {
			entry.LastSeen = sample.At
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
		delete(s.index, previous.key)
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
	s.index[key] = index
}

func decodeSample(record layers.SFlowRawPacketFlowRecord, rate uint32, now time.Time) Sample {
	sample := Sample{At: now, LastSeen: now, Bytes: int64(record.FrameLength) * int64(rate), Packets: int64(rate), Samples: 1}
	if layer := record.Header.Layer(layers.LayerTypeEthernet); layer != nil {
		frame := layer.(*layers.Ethernet)
		sample.SourceMAC, sample.DestinationMAC = frame.SrcMAC.String(), frame.DstMAC.String()
		sample.Source, sample.Destination, sample.Protocol = sample.SourceMAC, sample.DestinationMAC, frame.EthernetType.String()
	}
	if layer := record.Header.Layer(layers.LayerTypeIPv4); layer != nil {
		ip := layer.(*layers.IPv4)
		sample.Source, sample.Destination, sample.Protocol = ip.SrcIP.String(), ip.DstIP.String(), ip.Protocol.String()
	}
	if layer := record.Header.Layer(layers.LayerTypeIPv6); layer != nil {
		ip := layer.(*layers.IPv6)
		sample.Source, sample.Destination, sample.Protocol = ip.SrcIP.String(), ip.DstIP.String(), ip.NextHeader.String()
	}
	if layer := record.Header.Layer(layers.LayerTypeTCP); layer != nil {
		tcp := layer.(*layers.TCP)
		sample.Protocol, sample.SourcePort, sample.DestinationPort = "TCP", int(tcp.SrcPort), int(tcp.DstPort)
	}
	if layer := record.Header.Layer(layers.LayerTypeUDP); layer != nil {
		udp := layer.(*layers.UDP)
		sample.Protocol, sample.SourcePort, sample.DestinationPort = "UDP", int(udp.SrcPort), int(udp.DstPort)
	}
	return sample
}

func (s *Sampler) Snapshot(ctx context.Context, interfaces []api.CaptureInterface, now time.Time) (api.TrafficObservation, error) {
	ports, err := s.ovs.SamplingInterfaces(ctx)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return api.TrafficObservation{}, err
	}
	return s.snapshot(interfaces, ports, now)
}

func (s *Sampler) snapshot(interfaces []api.CaptureInterface, resolved map[uint32]string, now time.Time) (api.TrafficObservation, error) {
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
				if ports[port.resolve(resolved)] {
					omitted += count
				}
			}
		}
	}
	for index := range s.samples {
		observed := &s.samples[index]
		if observed.PortName == "" {
			observed.PortName = observed.key.port.resolve(resolved)
		}
		if ports[observed.PortName] && now.Sub(observed.LastSeen) <= ObservationWindow*time.Second {
			samples = append(samples, observed.Sample)
		}
	}
	s.mu.Unlock()
	flows, excluded := Summarize(s.node, interfaces, samples, now)
	return api.TrafficObservation{Flows: flows, Errors: map[string]string{}, ObservedAt: now, SamplingRate: SamplingRate, WindowSeconds: ObservationWindow, OmittedSamples: omitted + excluded}, nil
}
