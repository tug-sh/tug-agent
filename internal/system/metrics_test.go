package system

import "testing"

func TestDiskStatsReportsRootFilesystem(t *testing.T) {
	free, total, err := DiskStats("/")
	if err != nil {
		t.Fatalf("DiskStats failed: %v", err)
	}
	if total == 0 {
		t.Fatal("expected a non-zero total size for /")
	}
	if free > total {
		t.Fatalf("free (%d) cannot exceed total (%d)", free, total)
	}
}

// A missing path must not fail the caller: metrics fall back to the root
// filesystem so a handshake never breaks over an unmounted data directory.
func TestDiskStatsFallsBackForMissingPath(t *testing.T) {
	_, total, err := DiskStats("/definitely/not/a/real/path")
	if err != nil {
		t.Fatalf("DiskStats failed: %v", err)
	}
	if total == 0 {
		t.Fatal("expected the root filesystem fallback to report a size")
	}
}

func TestRAMUsage(t *testing.T) {
	used, total, percent, err := RAMUsage()
	if err != nil {
		t.Fatalf("RAMUsage failed: %v", err)
	}
	if total == 0 || used > total {
		t.Fatalf("implausible memory reading: used=%d total=%d", used, total)
	}
	if percent < 0 || percent > 100 {
		t.Fatalf("percent out of range: %f", percent)
	}
}

func TestCPUUsagePercent(t *testing.T) {
	percent, err := CPUUsagePercent()
	if err != nil {
		t.Fatalf("CPUUsagePercent failed: %v", err)
	}
	if percent < 0 || percent > 100 {
		t.Fatalf("percent out of range: %f", percent)
	}
}
