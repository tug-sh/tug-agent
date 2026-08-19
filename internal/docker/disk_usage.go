package docker

import (
	"context"
	"fmt"
	"strings"
)

type DiskUsageItem struct {
	Type        string `json:"type"`
	TotalCount  int    `json:"total_count"`
	ActiveCount int    `json:"active_count"`
	Size        string `json:"size"`
	Reclaimable string `json:"reclaimable"`
}

type DiskUsageReport struct {
	Images     DiskUsageItem `json:"images"`
	Containers DiskUsageItem `json:"containers"`
	Volumes    DiskUsageItem `json:"volumes"`
	BuildCache DiskUsageItem `json:"build_cache"`
	RawOutput  string        `json:"raw_output"`
}

func (manager *Manager) Prune(ctx context.Context) ([]string, error) {
	pruned, err := combined(ctx, "system", "prune", "-f")
	lines := strings.Split(strings.TrimSpace(pruned), "\n")
	if err != nil {
		return lines, fmt.Errorf("docker system prune failed: %s: %w", pruned, err)
	}
	return lines, nil
}

func (manager *Manager) DiskUsage(ctx context.Context) (DiskUsageReport, error) {
	usage, err := combined(ctx, "system", "df")
	if err != nil {
		return DiskUsageReport{}, fmt.Errorf("docker system df failed: %s: %w", usage, err)
	}

	report := DiskUsageReport{RawOutput: usage}
	for _, line := range strings.Split(usage, "\n") {
		item, ok := parseDiskUsageLine(line)
		if !ok {
			continue
		}
		switch itemType := strings.ToLower(item.Type); {
		case strings.Contains(itemType, "image"):
			report.Images = item
		case strings.Contains(itemType, "container"):
			report.Containers = item
		case strings.Contains(itemType, "volume"):
			report.Volumes = item
		case strings.Contains(itemType, "build"):
			report.BuildCache = item
		}
	}

	return report, nil
}

func parseDiskUsageLine(raw string) (DiskUsageItem, bool) {
	line := strings.TrimSpace(raw)
	if line == "" || strings.HasPrefix(line, "TYPE") {
		return DiskUsageItem{}, false
	}
	fields := strings.Fields(line)
	if len(fields) < 5 {
		return DiskUsageItem{}, false
	}
	item := DiskUsageItem{
		Type:        fields[0],
		Size:        fields[3],
		Reclaimable: strings.Join(fields[4:], " "),
	}
	fmt.Sscanf(fields[1], "%d", &item.TotalCount)
	fmt.Sscanf(fields[2], "%d", &item.ActiveCount)
	return item, true
}
