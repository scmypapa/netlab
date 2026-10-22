//go:build linux

package metrics

import (
	"fmt"

	"github.com/prometheus/procfs"
	"github.com/prometheus/procfs/blockdevice"
	"golang.org/x/sys/unix"
)

func Host(node, directory string) ([]Sample, error) {
	proc, err := procfs.NewFS("/proc")
	if err != nil {
		return nil, err
	}
	stat, err := proc.Stat()
	if err != nil {
		return nil, err
	}
	memory, err := proc.Meminfo()
	if err != nil {
		return nil, err
	}
	if memory.MemTotalBytes == nil || memory.MemAvailableBytes == nil {
		return nil, fmt.Errorf("/proc/meminfo is missing MemTotal or MemAvailable")
	}
	var space unix.Statfs_t
	if err = unix.Statfs(directory, &space); err != nil {
		return nil, err
	}
	var device unix.Stat_t
	if err = unix.Stat(directory, &device); err != nil {
		return nil, err
	}
	blocks, err := blockdevice.NewFS("/proc", "/sys")
	if err != nil {
		return nil, err
	}
	disks, err := blocks.ProcDiskstats()
	if err != nil {
		return nil, err
	}
	return hostSamples(node, stat, memory, space, device.Dev, disks), nil
}

func hostSamples(node string, stat procfs.Stat, memory procfs.Meminfo, space unix.Statfs_t, device uint64, disks []blockdevice.Diskstats) []Sample {
	cpu := stat.CPUTotal
	values := map[string]float64{
		"cpu_seconds_total":      cpu.User + cpu.Nice + cpu.System + cpu.IRQ + cpu.SoftIRQ + cpu.Steal,
		"cpu_capacity":           float64(len(stat.CPU)),
		"memory_bytes":           float64(*memory.MemTotalBytes - *memory.MemAvailableBytes),
		"memory_capacity_bytes":  float64(*memory.MemTotalBytes),
		"memory_available_bytes": float64(*memory.MemAvailableBytes),
		"disk_used_bytes":        float64(space.Blocks-space.Bfree) * float64(space.Bsize),
		"disk_capacity_bytes":    float64(space.Blocks) * float64(space.Bsize),
	}
	// Count the managed filesystem's device once, not both an LVM device and its disks.
	for _, disk := range disks {
		if disk.MajorNumber == unix.Major(device) && disk.MinorNumber == unix.Minor(device) {
			values["disk_read_bytes_total"] = float64(disk.ReadSectors) * 512
			values["disk_write_bytes_total"] = float64(disk.WriteSectors) * 512
			break
		}
	}
	samples := make([]Sample, 0, len(values))
	for name, value := range values {
		samples = append(samples, Sample{Name: name, Node: node, Value: value})
	}
	return samples
}
