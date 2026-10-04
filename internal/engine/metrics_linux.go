//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"

	cgroupstats "github.com/containerd/cgroups/v3/cgroup2/stats"
	"github.com/containerd/containerd"
	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/typeurl/v2"
	"github.com/vishvananda/netlink"
	"libvirt.org/go/libvirt"
	"libvirt.org/go/libvirtxml"
	"netlab.local/core/api"
	"netlab.local/core/internal/metrics"
)

func (e *Engine) Metrics(ctx context.Context) ([]metrics.Sample, error) {
	samples := []metrics.Sample{}
	if e.container != nil {
		ctx = namespaces.WithNamespace(ctx, "netlab")
		items, err := e.container.client.Containers(ctx, "labels.\""+assetLabel+"\"")
		if err != nil {
			return nil, err
		}
		identities := make(map[string]map[string]string, len(items))
		interfaces := map[string]api.AssetExecution{}
		for _, item := range items {
			info, err := item.Info(ctx, containerd.WithoutRefreshedMetadata)
			if err != nil {
				return nil, err
			}
			if !managedContainer(info.Labels, e.cfg.ID) {
				continue
			}
			identities[item.ID()] = info.Labels
			var execution api.AssetExecution
			if err = json.Unmarshal([]byte(info.Labels[executionLabel]), &execution); err != nil {
				return nil, err
			}
			interfaces[item.ID()] = execution
		}
		batch, err := e.container.client.TaskService().Metrics(ctx, &tasksapi.MetricsRequest{})
		if err != nil {
			return nil, err
		}
		links, err := netlink.LinkList()
		if err != nil {
			return nil, err
		}
		linkStats := map[string]*netlink.LinkStatistics{}
		for _, link := range links {
			linkStats[link.Attrs().Name] = link.Attrs().Statistics
		}
		for _, item := range batch.Metrics {
			labels, owned := identities[item.ID]
			if !owned {
				continue
			}
			decoded, err := typeurl.UnmarshalAny(item.Data)
			if err != nil {
				return nil, err
			}
			stats, supported := decoded.(*cgroupstats.Metrics)
			if !supported {
				return nil, fmt.Errorf("container metrics require cgroup v2, got %T", decoded)
			}
			base := metrics.Sample{Environment: labels[environmentLabel], Asset: labels[assetLabel], Instance: item.ID, Node: e.cfg.ID}
			if stats.CPU != nil {
				samples = append(samples, measurement(base, "cpu_seconds_total", float64(stats.CPU.UsageUsec)/1e6))
			}
			if stats.Memory != nil {
				samples = append(samples, measurement(base, "memory_bytes", float64(stats.Memory.Usage)))
			}
			if stats.Io != nil {
				var read, written uint64
				for _, usage := range stats.Io.Usage {
					read += usage.Rbytes
					written += usage.Wbytes
				}
				samples = append(samples, measurement(base, "disk_read_bytes_total", float64(read)), measurement(base, "disk_write_bytes_total", float64(written)))
			}
			for _, iface := range interfaces[item.ID].Interfaces {
				if counters := linkStats[deviceName(iface.PortName)]; counters != nil {
					base.Interface = iface.Id
					// Host veth counters are the opposite direction of the guest interface.
					samples = append(samples, interfaceMetrics(base, counters.TxBytes, counters.RxBytes, counters.TxPackets, counters.RxPackets, counters.TxDropped, counters.RxDropped)...)
				}
			}
		}
	}
	if e.vm != nil {
		batch, err := e.vm.conn.GetAllDomainStats(nil, libvirt.DOMAIN_STATS_CPU_TOTAL|libvirt.DOMAIN_STATS_BALLOON|libvirt.DOMAIN_STATS_BLOCK|libvirt.DOMAIN_STATS_INTERFACE, libvirt.CONNECT_GET_ALL_DOMAINS_STATS_ACTIVE)
		if err != nil {
			return nil, err
		}
		defer func() {
			for _, stats := range batch {
				stats.Domain.Free()
			}
		}()
		for _, stats := range batch {
			values, err := e.vm.domainMetrics(stats, e.cfg.ID)
			if err != nil {
				return nil, err
			}
			samples = append(samples, values...)
		}
	}
	return samples, nil
}

func measurement(base metrics.Sample, name string, value float64) metrics.Sample {
	base.Name, base.Value = name, value
	return base
}

func interfaceMetrics(base metrics.Sample, rx, tx, rxPackets, txPackets, rxDrops, txDrops uint64) []metrics.Sample {
	return []metrics.Sample{
		measurement(base, "interface_receive_bytes_total", float64(rx)), measurement(base, "interface_transmit_bytes_total", float64(tx)),
		measurement(base, "interface_receive_packets_total", float64(rxPackets)), measurement(base, "interface_transmit_packets_total", float64(txPackets)),
		measurement(base, "interface_receive_drops_total", float64(rxDrops)), measurement(base, "interface_transmit_drops_total", float64(txDrops)),
	}
}

func (v *VirtualMachines) domainMetrics(stats libvirt.DomainStats, node string) ([]metrics.Sample, error) {
	text, err := stats.Domain.GetXMLDesc(0)
	if noDomain(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var config libvirtxml.Domain
	if err = config.Unmarshal(text); err != nil {
		return nil, err
	}
	if config.Metadata == nil {
		return nil, nil
	}
	var owner Ownership
	if err = xml.Unmarshal([]byte(config.Metadata.XML), &owner); err != nil || owner.XMLName.Space != "urn:netlab:instance" {
		return nil, nil
	}
	var execution api.AssetExecution
	if err = json.Unmarshal([]byte(owner.Execution), &execution); err != nil {
		return nil, err
	}
	base := metrics.Sample{Environment: owner.Environment, Asset: owner.Asset, Instance: owner.Instance, Node: node}
	samples := []metrics.Sample{}
	if stats.Cpu != nil && stats.Cpu.TimeSet {
		samples = append(samples, measurement(base, "cpu_seconds_total", float64(stats.Cpu.Time)/1e9))
	}
	if stats.Balloon != nil && stats.Balloon.RssSet {
		samples = append(samples, measurement(base, "memory_bytes", float64(stats.Balloon.Rss)*1024))
	}
	var read, written uint64
	var readSet, writeSet bool
	for _, block := range stats.Block {
		if block.BackingIndexSet && block.BackingIndex > 0 {
			continue
		}
		read += block.RdBytes
		written += block.WrBytes
		readSet = readSet || block.RdBytesSet
		writeSet = writeSet || block.WrBytesSet
	}
	if readSet {
		samples = append(samples, measurement(base, "disk_read_bytes_total", float64(read)))
	}
	if writeSet {
		samples = append(samples, measurement(base, "disk_write_bytes_total", float64(written)))
	}
	ports := map[string]string{}
	for _, iface := range execution.Interfaces {
		ports[iface.PortName] = iface.Id
	}
	devices := map[string]string{}
	for _, iface := range config.Devices.Interfaces {
		if iface.Target != nil && iface.VirtualPort != nil && iface.VirtualPort.Params != nil && iface.VirtualPort.Params.OpenVSwitch != nil {
			devices[iface.Target.Dev] = ports[iface.VirtualPort.Params.OpenVSwitch.InterfaceID]
		}
	}
	for _, counters := range stats.Net {
		if iface := devices[counters.Name]; iface != "" {
			base.Interface = iface
			for _, counter := range []struct {
				name      string
				value     uint64
				available bool
			}{
				{"interface_receive_bytes_total", counters.RxBytes, counters.RxBytesSet}, {"interface_transmit_bytes_total", counters.TxBytes, counters.TxBytesSet},
				{"interface_receive_packets_total", counters.RxPkts, counters.RxPktsSet}, {"interface_transmit_packets_total", counters.TxPkts, counters.TxPktsSet},
				{"interface_receive_drops_total", counters.RxDrop, counters.RxDropSet}, {"interface_transmit_drops_total", counters.TxDrop, counters.TxDropSet},
			} {
				if counter.available {
					samples = append(samples, measurement(base, counter.name, float64(counter.value)))
				}
			}
		}
	}
	return samples, nil
}
