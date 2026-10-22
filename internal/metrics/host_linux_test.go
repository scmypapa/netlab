//go:build linux

package metrics

import (
	"testing"

	"github.com/prometheus/procfs"
	"github.com/prometheus/procfs/blockdevice"
	"golang.org/x/sys/unix"
)

func TestHostSamples(t *testing.T) {
	total, available := uint64(16<<30), uint64(10<<30)
	stat := procfs.Stat{CPUTotal: procfs.CPUStat{User: 8, System: 4, Idle: 40, Iowait: 2, Guest: 3}, CPU: map[int64]procfs.CPUStat{0: {}, 1: {}}}
	memory := procfs.Meminfo{MemTotalBytes: &total, MemAvailableBytes: &available}
	disks := []blockdevice.Diskstats{
		{Info: blockdevice.Info{MajorNumber: 8, MinorNumber: 0}, IOStats: blockdevice.IOStats{ReadSectors: 999}},
		{Info: blockdevice.Info{MajorNumber: 253, MinorNumber: 0}, IOStats: blockdevice.IOStats{ReadSectors: 5, WriteSectors: 7}},
	}
	samples := hostSamples("node", stat, memory, unix.Statfs_t{Blocks: 100, Bfree: 40, Bsize: 4096}, unix.Mkdev(253, 0), disks)
	want := map[string]float64{"cpu_seconds_total": 12, "cpu_capacity": 2, "memory_bytes": 6 << 30, "memory_available_bytes": 10 << 30, "memory_capacity_bytes": 16 << 30, "disk_used_bytes": 60 * 4096, "disk_capacity_bytes": 100 * 4096, "disk_read_bytes_total": 5 * 512, "disk_write_bytes_total": 7 * 512}
	for _, sample := range samples {
		if sample.Node != "node" || sample.Environment != "" || sample.Asset != "" || sample.Instance != "" {
			t.Fatalf("host sample has asset identity: %+v", sample)
		}
		if expected, exists := want[sample.Name]; !exists || sample.Value != expected {
			t.Errorf("%s = %f, want %f", sample.Name, sample.Value, expected)
		}
		delete(want, sample.Name)
	}
	if len(want) != 0 {
		t.Errorf("missing metrics: %v", want)
	}
}

func TestHostMetrics(t *testing.T) {
	samples, err := Host("node", t.TempDir())
	if err != nil || len(samples) < 7 {
		t.Fatalf("host metrics: %v, %v", samples, err)
	}
}
