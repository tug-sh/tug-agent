package agent

import (
	"fmt"
	"io"
	"syscall"

	"github.com/shirou/gopsutil/v4/mem"
)

func ioReadAll(reader io.Reader) ([]byte, error) {
	return io.ReadAll(reader)
}

func detectDiskFree(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		if path != "/" {
			return detectDiskFree("/")
		}
		return 0, fmt.Errorf("cannot read disk stats: %w", err)
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}

func detectTotalRAMBytes() (uint64, error) {
	stats, err := mem.VirtualMemory()
	if err != nil {
		return 0, fmt.Errorf("cannot read memory stats: %w", err)
	}
	return stats.Total, nil
}
