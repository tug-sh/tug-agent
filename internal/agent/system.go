package agent

import (
	"fmt"
	"io"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
)

func ioReadAll(reader io.Reader) ([]byte, error) {
	return io.ReadAll(reader)
}

func detectDiskFree(path string) (uint64, error) {
	free, _, err := detectDiskStats(path)
	return free, err
}

func detectDiskStats(path string) (freeBytes uint64, totalBytes uint64, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		if path != "/" {
			return detectDiskStats("/")
		}
		return 0, 0, fmt.Errorf("cannot read disk stats: %w", err)
	}
	return stat.Bavail * uint64(stat.Bsize), stat.Blocks * uint64(stat.Bsize), nil
}

func detectTotalRAMBytes() (uint64, error) {
	stats, err := mem.VirtualMemory()
	if err != nil {
		return 0, fmt.Errorf("cannot read memory stats: %w", err)
	}
	return stats.Total, nil
}

func detectCPUUsagePct() (float64, error) {
	percentages, err := cpu.Percent(100*time.Millisecond, false)
	if err != nil || len(percentages) == 0 {
		return 0, err
	}
	return percentages[0], nil
}

func detectRAMUsage() (usedBytes uint64, totalBytes uint64, percent float64, err error) {
	stats, err := mem.VirtualMemory()
	if err != nil {
		return 0, 0, 0, err
	}
	return stats.Used, stats.Total, stats.UsedPercent, nil
}
