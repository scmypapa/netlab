//go:build linux

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

type sampleDrop struct{ second, count int64 }
type Sampler struct {
	ovs        *network.OVS
	node       string
	conn       *net.UDPConn
	cancel     context.CancelFunc
	workers    sync.WaitGroup
	mu         sync.Mutex
	interfaces map[uint32]string
	samples    []Sample
	next       int
	omitted    map[string][ObservationWindow]sampleDrop
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
	ctx, cancel := context.WithCancel(ctx)
	s := &Sampler{ovs: ovs, node: node, conn: conn, cancel: cancel, interfaces: interfaces, omitted: map[string][ObservationWindow]sampleDrop{}}
	s.workers.Add(2)
	go func() { defer s.workers.Done(); s.receive() }()
	go func() { defer s.workers.Done(); s.refresh(ctx) }()
	return s, nil
}

func (s *Sampler) Close() error {
	s.cancel()
	s.conn.Close()
	s.workers.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.ovs.SetSampling(ctx, s.node, "", SamplingRate)
}

func (s *Sampler) refresh(ctx context.Context) {
	timer := time.NewTicker(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			interfaces, err := s.ovs.SamplingInterfaces(ctx)
			s.mu.Lock()
			s.interfaces, s.err = interfaces, err
			active := map[string]bool{}
			for _, port := range interfaces {
				active[port] = true
			}
			for port := range s.omitted {
				if !active[port] {
					delete(s.omitted, port)
				}
			}
			s.mu.Unlock()
		}
	}
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
				s.cancel()
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
	for _, flow := range datagram.FlowSamples {
		port := ""
		if flow.InputInterfaceFormat == 0 {
			port = s.interfaces[flow.InputInterface]
		}
		if port == "" && flow.OutputInterfaceFormat == 0 {
			port = s.interfaces[flow.OutputInterface]
		}
		if port == "" || flow.SamplingRate == 0 {
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
			sample.PortName = port
			if len(s.samples) < sampleBudget {
				s.samples = append(s.samples, sample)
			} else {
				previous := s.samples[s.next]
				if now.Sub(previous.At) < ObservationWindow*time.Second {
					buckets := s.omitted[previous.PortName]
					second := previous.At.Unix()
					index := second % ObservationWindow
					if buckets[index].second != second {
						buckets[index] = sampleDrop{second: second}
					}
					buckets[index].count++
					s.omitted[previous.PortName] = buckets
				}
				s.samples[s.next] = sample
				s.next = (s.next + 1) % sampleBudget
			}
			break
		}
	}
	return nil
}

func decodeSample(record layers.SFlowRawPacketFlowRecord, rate uint32, now time.Time) Sample {
	sample := Sample{At: now, Bytes: int64(record.FrameLength) * int64(rate), Packets: int64(rate)}
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

func (s *Sampler) Snapshot(interfaces []api.CaptureInterface, now time.Time) (api.TrafficObservation, error) {
	ports := map[string]bool{}
	for _, iface := range interfaces {
		if iface.NodeId == s.node {
			ports[iface.PortName] = true
		}
	}
	s.mu.Lock()
	if s.err != nil {
		err := s.err
		s.mu.Unlock()
		return api.TrafficObservation{}, err
	}
	var samples []Sample
	var omitted int64
	for port := range ports {
		for _, drop := range s.omitted[port] {
			if drop.second > now.Unix()-ObservationWindow {
				omitted += drop.count
			}
		}
	}
	for _, observed := range s.samples {
		if ports[observed.PortName] && now.Sub(observed.At) <= ObservationWindow*time.Second {
			samples = append(samples, observed)
		}
	}
	s.mu.Unlock()
	flows, excluded := Summarize(s.node, interfaces, samples, now)
	return api.TrafficObservation{Flows: flows, Errors: map[string]string{}, ObservedAt: now, SamplingRate: SamplingRate, WindowSeconds: ObservationWindow, OmittedSamples: omitted + excluded}, nil
}
