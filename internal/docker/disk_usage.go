package docker

import (
	"context"
	"encoding/json"
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

type rawDiskUsageJSONItem struct {
	Type        string `json:"Type"`
	TotalCount  string `json:"TotalCount"`
	Active      string `json:"Active"`
	Size        string `json:"Size"`
	Reclaimable string `json:"Reclaimable"`
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
	// First attempt: structured json output from docker system df
	jsonOut, jsonErr := combined(ctx, "system", "df", "--format", "{{json .}}")
	if jsonErr == nil && strings.TrimSpace(jsonOut) != "" && strings.HasPrefix(strings.TrimSpace(jsonOut), "{") {
		report, parseErr := parseDiskUsageJSON(jsonOut)
		if parseErr == nil && (report.Images.Size != "" || report.Containers.Size != "" || report.Volumes.Size != "") {
			raw, _ := combined(ctx, "system", "df")
			report.RawOutput = raw
			return report, nil
		}
	}

	// Fallback to table output
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

func parseDiskUsageJSON(raw string) (DiskUsageReport, error) {
	var report DiskUsageReport
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var rawItem rawDiskUsageJSONItem
		if err := json.Unmarshal([]byte(line), &rawItem); err != nil {
			continue
		}
		item := DiskUsageItem{
			Type:        rawItem.Type,
			Size:        rawItem.Size,
			Reclaimable: rawItem.Reclaimable,
		}
		fmt.Sscanf(rawItem.TotalCount, "%d", &item.TotalCount)
		fmt.Sscanf(rawItem.Active, "%d", &item.ActiveCount)

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
	if line == "" || strings.HasPrefix(strings.ToUpper(line), "TYPE") {
		return DiskUsageItem{}, false
	}
	fields := strings.Fields(line)
	if len(fields) < 4 {
		return DiskUsageItem{}, false
	}

	// Find the first integer field (which corresponds to TOTAL count)
	intIdx := -1
	for i := 1; i < len(fields)-2; i++ {
		var n int
		if _, err := fmt.Sscanf(fields[i], "%d", &n); err == nil {
			intIdx = i
			break
		}
	}
	if intIdx == -1 || intIdx+2 >= len(fields) {
		return DiskUsageItem{}, false
	}

	reclaimable := ""
	if intIdx+3 < len(fields) {
		reclaimable = strings.Join(fields[intIdx+3:], " ")
	}

	item := DiskUsageItem{
		Type:        strings.Join(fields[:intIdx], " "),
		Size:        fields[intIdx+2],
		Reclaimable: reclaimable,
	}
	fmt.Sscanf(fields[intIdx], "%d", &item.TotalCount)
	fmt.Sscanf(fields[intIdx+1], "%d", &item.ActiveCount)
	return item, true
}
