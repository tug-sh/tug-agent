// Package system reads host level metrics reported in handshakes and heartbeats.
package system

import (
	"fmt"
	"syscall"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
)

// DiskStats reports free and total bytes for the filesystem holding path,
// falling back to the root filesystem when path cannot be inspected.
func DiskStats(path string) (freeBytes uint64, totalBytes uint64, err error) {
	var stat syscall.Statfs_t
	if statErr := syscall.Statfs(path, &stat); statErr != nil {
		if path != "/" {
			return DiskStats("/")
		}
		return 0, 0, fmt.Errorf("cannot read disk stats: %w", statErr)
	}
	return stat.Bavail * uint64(stat.Bsize), stat.Blocks * uint64(stat.Bsize), nil
}

func CPUUsagePercent() (float64, error) {
	percentages, err := cpu.Percent(0, false)
	if err != nil || len(percentages) == 0 {
		return 0, err
	}
	return percentages[0], nil
}

func RAMUsage() (usedBytes uint64, totalBytes uint64, percent float64, err error) {
	stats, err := mem.VirtualMemory()
	if err != nil {
		return 0, 0, 0, err
	}
	return stats.Used, stats.Total, stats.UsedPercent, nil
}
