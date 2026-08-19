package docker

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"tug.sh/services/agent/internal/protocol"
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
		if len(parts) < 8 {
			continue
		}
		containerID := strings.TrimSpace(parts[0])
		logsPreview := []string{}
		if includeLogsPreview {
			logsPreview, _ = manager.GetLogsPreview(ctx, containerID, defaultLogsPreviewLines)
		}
		containers = append(containers, protocol.HandshakeContainer{
			ID:          containerID,
			Name:        strings.TrimSpace(parts[1]),
			Image:       strings.TrimSpace(parts[2]),
			Ports:       strings.TrimSpace(parts[3]),
			Status:      normalizeContainerStatus(parts[4]),
			Networks:    splitCSV(parts[5]),
			ProjectID:   strings.TrimSpace(parts[6]),
			App:         strings.TrimSpace(parts[7]),
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
