package docker

import "testing"

func TestParseDiskUsageLine(t *testing.T) {
	item, ok := parseDiskUsageLine("Images          12        4         3.5GB     2.1GB (60%)")
	if !ok {
		t.Fatal("expected the line to parse")
	}
	if item.Type != "Images" {
		t.Errorf("Type = %q, want %q", item.Type, "Images")
	}
	if item.TotalCount != 12 || item.ActiveCount != 4 {
		t.Errorf("counts = %d/%d, want 12/4", item.TotalCount, item.ActiveCount)
	}
	if item.Size != "3.5GB" {
		t.Errorf("Size = %q, want %q", item.Size, "3.5GB")
	}
	if item.Reclaimable != "2.1GB (60%)" {
		t.Errorf("Reclaimable = %q, want %q", item.Reclaimable, "2.1GB (60%)")
	}
}

func TestParseDiskUsageLineSkipsNonData(t *testing.T) {
	skipped := []string{
		"",
		"   ",
		"TYPE            TOTAL     ACTIVE    SIZE      RECLAIMABLE",
		"Build Cache     0",
	}
	for _, line := range skipped {
		if _, ok := parseDiskUsageLine(line); ok {
			t.Errorf("expected %q to be skipped", line)
		}
	}
}
