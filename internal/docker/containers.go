package docker

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"tug.sh/pkg/protocol"
)

const defaultLogsPreviewLines = 20

func (manager *Manager) StreamEvents(ctx context.Context, onEvent func(raw []byte) error) error {
	cmd := exec.CommandContext(ctx, "docker", "events", "--format", "{{json .}}")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer cmd.Process.Kill()

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Bytes()
		payload := make([]byte, len(line))
		copy(payload, line)
		if err := onEvent(payload); err != nil {
			return err
		}
	}

	return scanner.Err()
}

// ListContainers returns every container including a short log preview, which
// costs one extra docker call per container.
func (manager *Manager) ListContainers(ctx context.Context) ([]protocol.HandshakeContainer, error) {
	return manager.listContainers(ctx, true)
}

// ListContainersLite skips log previews and is used on hot paths such as
// docker event deltas.
func (manager *Manager) ListContainersLite(ctx context.Context) ([]protocol.HandshakeContainer, error) {
	return manager.listContainers(ctx, false)
}

func (manager *Manager) listContainers(
	ctx context.Context,
	includeLogsPreview bool,
) ([]protocol.HandshakeContainer, error) {
	listed, err := output(
		ctx,
		"ps",
		"-a",
		"--format",
		"{{.ID}}\t{{.Names}}\t{{.Image}}\t{{.Ports}}\t{{.Status}}\t{{.Networks}}\t{{.Label \"com.docker.compose.project\"}}\t{{.Label \"tug.app\"}}",
	)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(listed), "\n")
	if len(lines) == 1 && strings.TrimSpace(lines[0]) == "" {
		return []protocol.HandshakeContainer{}, nil
	}

	containers := make([]protocol.HandshakeContainer, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 5 {
			continue
		}
		containerID := strings.TrimSpace(parts[0])
		name := strings.TrimSpace(parts[1])
		image := strings.TrimSpace(parts[2])
		ports := strings.TrimSpace(parts[3])
		status := normalizeContainerStatus(parts[4])
		networks := []string{}
		if len(parts) > 5 {
			networks = splitCSV(parts[5])
		}
		projectID := ""
		if len(parts) > 6 {
			projectID = strings.TrimSpace(parts[6])
		}
		app := ""
		if len(parts) > 7 {
			app = strings.TrimSpace(parts[7])
		}

		logsPreview := []string{}
		if includeLogsPreview {
			logsPreview, _ = manager.GetLogsPreview(ctx, containerID, defaultLogsPreviewLines)
		}
		containers = append(containers, protocol.HandshakeContainer{
			ID:          containerID,
			Name:        name,
			Image:       image,
			Ports:       ports,
			Status:      status,
			Networks:    networks,
			ProjectID:   projectID,
			App:         app,
			LogsPreview: logsPreview,
		})
	}

	return containers, nil
}

func (manager *Manager) ControlContainer(
	ctx context.Context,
	containerID string,
	action string,
	removeVolumes bool,
	removeImage bool,
) error {
	containerID = strings.TrimSpace(containerID)
	action = strings.TrimSpace(strings.ToLower(action))
	if containerID == "" {
		return fmt.Errorf("container_id is required")
	}
	if action != "start" && action != "stop" && action != "restart" && action != "remove" {
		return fmt.Errorf("unsupported action %s", action)
	}

	var imageName string
	if action == "remove" && removeImage {
		if image, err := output(ctx, "inspect", "--format={{.Config.Image}}", containerID); err == nil {
			imageName = strings.TrimSpace(image)
		}
	}

	args := []string{action, containerID}
	if action == "remove" {
		args = []string{"rm", "-f"}
		if removeVolumes {
			args = append(args, "-v")
		}
		args = append(args, containerID)
	}

	if result, err := combined(ctx, args...); err != nil {
		return fmt.Errorf("docker %s failed: %s: %w", action, result, err)
	}

	if action == "remove" && removeImage && imageName != "" {
		_, _ = combined(ctx, "rmi", imageName)
	}

	return nil
}

// RenameContainer changes the Docker name of a single container. Compose still
// owns the service name, so a later recreate reverts to the compose-derived
// name; this is a live relabel, not a permanent redefinition.
func (manager *Manager) RenameContainer(ctx context.Context, containerID, newName string) error {
	containerID = strings.TrimSpace(containerID)
	newName = strings.TrimSpace(newName)
	if containerID == "" {
		return fmt.Errorf("container_id is required")
	}
	if newName == "" {
		return fmt.Errorf("new name is required")
	}
	if result, err := combined(ctx, "rename", containerID, newName); err != nil {
		return fmt.Errorf("docker rename failed: %s: %w", strings.TrimSpace(result), err)
	}
	return nil
}

func (manager *Manager) GetLogsPreview(
	ctx context.Context,
	containerID string,
	lineLimit int,
) ([]string, error) {
	if strings.TrimSpace(containerID) == "" {
		return []string{}, nil
	}
	if lineLimit <= 0 {
		lineLimit = defaultLogsPreviewLines
	}
	logs, err := combined(ctx, "logs", "--tail", fmt.Sprintf("%d", lineLimit), containerID)
	if err != nil {
		return []string{}, err
	}
	raw := strings.TrimSpace(logs)
	if raw == "" {
		return []string{}, nil
	}
	return strings.Split(raw, "\n"), nil
}

func normalizeContainerStatus(raw string) string {
	status := strings.ToLower(strings.TrimSpace(raw))
	if strings.HasPrefix(status, "up") {
		return "running"
	}
	return "stopped"
}

// CollectContainerStats executes docker stats --no-stream and returns parsed
// metrics for all running containers.
func (manager *Manager) CollectContainerStats(ctx context.Context) ([]protocol.ContainerMetric, error) {
	raw, err := output(
		ctx,
		"stats",
		"--no-stream",
		"--format",
		"{{.ID}}\t{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.NetIO}}\t{{.BlockIO}}",
	)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(raw), "\n")
	metrics := make([]protocol.ContainerMetric, 0, len(lines))

	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 6 {
			continue
		}

		containerID := strings.TrimSpace(parts[0])
		name := strings.TrimSpace(parts[1])
		cpuRaw := strings.TrimSuffix(strings.TrimSpace(parts[2]), "%")
		cpuVal, _ := strconv.ParseFloat(cpuRaw, 64)

		memUsed, memTotal := parseTwoSlashValues(parts[3])
		netRx, netTx := parseTwoSlashValues(parts[4])
		blockRead, blockWrite := parseTwoSlashValues(parts[5])

		metrics = append(metrics, protocol.ContainerMetric{
			ID:              containerID,
			Name:            name,
			Status:          "running",
			CPUPercent:      cpuVal,
			RAMUsedBytes:    memUsed,
			RAMTotalBytes:   memTotal,
			NetworkRxBytes:  netRx,
			NetworkTxBytes:  netTx,
			BlockReadBytes:  blockRead,
			BlockWriteBytes: blockWrite,
		})
	}

	return metrics, nil
}

func parseTwoSlashValues(raw string) (uint64, uint64) {
	parts := strings.Split(raw, "/")
	if len(parts) < 2 {
		return 0, 0
	}
	return parseByteSize(parts[0]), parseByteSize(parts[1])
}

// parseByteSize parses strings like "45.2MiB", "1.2GB", "500kB", "1024B" into bytes.
func parseByteSize(raw string) uint64 {
	s := strings.TrimSpace(raw)
	if s == "" || s == "0" || s == "--" {
		return 0
	}

	unitIdx := -1
	for i, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			unitIdx = i
			break
		}
	}

	if unitIdx == -1 {
		val, _ := strconv.ParseUint(s, 10, 64)
		return val
	}

	numStr := strings.TrimSpace(s[:unitIdx])
	unit := strings.ToUpper(strings.TrimSpace(s[unitIdx:]))

	num, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0
	}

	multiplier := float64(1)
	switch unit {
	case "B":
		multiplier = 1
	case "KB", "KIB", "K":
		multiplier = 1024
	case "MB", "MIB", "M":
		multiplier = 1024 * 1024
	case "GB", "GIB", "G":
		multiplier = 1024 * 1024 * 1024
	case "TB", "TIB", "T":
		multiplier = 1024 * 1024 * 1024 * 1024
	}

	return uint64(num * multiplier)
}
