package docker

import "testing"

func TestParseDiskUsageLine(t *testing.T) {
	tests := []struct {
		name        string
		line        string
		wantType    string
		wantTotal   int
		wantActive  int
		wantSize    string
		wantReclaim string
	}{
		{
			name:        "images",
			line:        "Images          12        4         3.5GB     2.1GB (60%)",
			wantType:    "Images",
			wantTotal:   12,
			wantActive:  4,
			wantSize:    "3.5GB",
			wantReclaim: "2.1GB (60%)",
		},
		{
			name:        "containers",
			line:        "Containers      9         2         331.9MB   331.9MB (99%)",
			wantType:    "Containers",
			wantTotal:   9,
			wantActive:  2,
			wantSize:    "331.9MB",
			wantReclaim: "331.9MB (99%)",
		},
		{
			name:        "local volumes multi-word",
			line:        "Local Volumes   36        3         1.27GB    973.8MB (76%)",
			wantType:    "Local Volumes",
			wantTotal:   36,
			wantActive:  3,
			wantSize:    "1.27GB",
			wantReclaim: "973.8MB (76%)",
		},
		{
			name:        "build cache multi-word",
			line:        "Build Cache     55        0         4.506GB   4.506GB",
			wantType:    "Build Cache",
			wantTotal:   55,
			wantActive:  0,
			wantSize:    "4.506GB",
			wantReclaim: "4.506GB",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item, ok := parseDiskUsageLine(tt.line)
			if !ok {
				t.Fatalf("expected line to parse: %q", tt.line)
			}
			if item.Type != tt.wantType {
				t.Errorf("Type = %q, want %q", item.Type, tt.wantType)
			}
			if item.TotalCount != tt.wantTotal || item.ActiveCount != tt.wantActive {
				t.Errorf("counts = %d/%d, want %d/%d", item.TotalCount, item.ActiveCount, tt.wantTotal, tt.wantActive)
			}
			if item.Size != tt.wantSize {
				t.Errorf("Size = %q, want %q", item.Size, tt.wantSize)
			}
			if item.Reclaimable != tt.wantReclaim {
				t.Errorf("Reclaimable = %q, want %q", item.Reclaimable, tt.wantReclaim)
			}
		})
	}
}

func TestParseDiskUsageJSON(t *testing.T) {
	raw := `{"Active":"8","Reclaimable":"17.56GB (87%)","Size":"20.01GB","TotalCount":"32","Type":"Images"}
{"Active":"2","Reclaimable":"331.9MB (99%)","Size":"331.9MB","TotalCount":"9","Type":"Containers"}
{"Active":"3","Reclaimable":"973.8MB (76%)","Size":"1.27GB","TotalCount":"36","Type":"Local Volumes"}
{"Active":"0","Reclaimable":"4.506GB","Size":"4.506GB","TotalCount":"55","Type":"Build Cache"}`

	report, err := parseDiskUsageJSON(raw)
	if err != nil {
		t.Fatalf("unexpected error parsing json: %v", err)
	}

	if report.Images.TotalCount != 32 || report.Images.ActiveCount != 8 || report.Images.Size != "20.01GB" {
		t.Errorf("unexpected Images: %+v", report.Images)
	}
	if report.Containers.TotalCount != 9 || report.Containers.ActiveCount != 2 || report.Containers.Size != "331.9MB" {
		t.Errorf("unexpected Containers: %+v", report.Containers)
	}
	if report.Volumes.TotalCount != 36 || report.Volumes.ActiveCount != 3 || report.Volumes.Size != "1.27GB" {
		t.Errorf("unexpected Volumes: %+v", report.Volumes)
	}
	if report.BuildCache.TotalCount != 55 || report.BuildCache.ActiveCount != 0 || report.BuildCache.Size != "4.506GB" {
		t.Errorf("unexpected BuildCache: %+v", report.BuildCache)
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
