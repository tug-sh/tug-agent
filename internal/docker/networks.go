package docker

import (
	"context"
	"fmt"
	"strings"

	"tug.sh/services/agent/internal/logging"
	"tug.sh/services/agent/internal/shell"
)

// EnsureNetwork creates the network unless docker already knows it.
func (manager *Manager) EnsureNetwork(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("network name is required")
	}
	if _, err := output(ctx, "network", "inspect", name); err == nil {
		return nil
	}
	created, err := combined(ctx, "network", "create", name)
	if err != nil {
		return fmt.Errorf("cannot create network %s: %s: %w", name, created, err)
	}
	return nil
}

func (manager *Manager) ListNetworks(ctx context.Context) ([]string, error) {
	listed, err := output(ctx, "network", "ls", "--format", "{{.Name}}")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(listed), "\n")
	names := make([]string, 0, len(lines))
	for _, line := range lines {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	return names, nil
}

func (manager *Manager) CreateNetwork(ctx context.Context, name string) ([]string, error) {
	transcript := logging.NewTranscript(fmt.Sprintf("Creating docker network %s...", name))
	if err := shell.RunTracked(ctx, transcript, "cannot create network", "docker", "network", "create", name); err != nil {
		return transcript.Lines(), err
	}
	return transcript.Done("Network created.")
}

func (manager *Manager) DeleteNetwork(ctx context.Context, name string) ([]string, error) {
	transcript := logging.NewTranscript(fmt.Sprintf("Removing docker network %s...", name))
	if err := shell.RunTracked(ctx, transcript, "cannot remove network", "docker", "network", "rm", name); err != nil {
		return transcript.Lines(), err
	}
	return transcript.Done("Network removed.")
}
