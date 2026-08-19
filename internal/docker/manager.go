// Package docker wraps the docker CLI. It exposes generic primitives only:
// nothing here knows which images or containers the platform runs, so callers
// such as the router own that policy.
package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"tug.sh/services/agent/internal/logging"
	"tug.sh/services/agent/internal/shell"
)

type Manager struct{}

func NewManager() *Manager {
	return &Manager{}
}

// combined runs a docker subcommand and returns stdout together with stderr,
// because failures are surfaced to the user as the command output.
func combined(ctx context.Context, args ...string) (string, error) {
	output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	return string(output), err
}

// output runs a docker subcommand and returns stdout only, for commands whose
// result is parsed rather than displayed.
func output(ctx context.Context, args ...string) (string, error) {
	stdout, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return "", err
	}
	return string(stdout), nil
}

// Inspect returns the raw docker inspect JSON for a container. The optional
// arguments narrow the result with a --format template.
func (manager *Manager) Inspect(
	ctx context.Context,
	containerID string,
	inspectArgs ...string,
) (json.RawMessage, error) {
	args := append(append([]string{"inspect"}, inspectArgs...), containerID)
	stdout, err := output(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("docker inspect failed: %w", err)
	}
	return json.RawMessage(stdout), nil
}

// PortMapping is a host:container port publication for a managed container.
type PortMapping struct {
	HostPort      int
	ContainerPort int
}

func (mapping PortMapping) String() string {
	return fmt.Sprintf("%d:%d", mapping.HostPort, mapping.ContainerPort)
}

// ContainerSpec describes a container the agent starts directly instead of
// through compose.
type ContainerSpec struct {
	Name          string
	Image         string
	Network       string
	RestartPolicy string
	Labels        map[string]string
	Ports         []PortMapping
}

// RunContainer starts a detached container and returns the docker output.
func (manager *Manager) RunContainer(ctx context.Context, spec ContainerSpec) (string, error) {
	if strings.TrimSpace(spec.Name) == "" {
		return "", fmt.Errorf("container name is required")
	}
	if strings.TrimSpace(spec.Image) == "" {
		return "", fmt.Errorf("container image is required")
	}

	args := []string{"run", "-d", "--name", spec.Name}
	for _, key := range sortedKeys(spec.Labels) {
		args = append(args, "--label", fmt.Sprintf("%s=%s", key, spec.Labels[key]))
	}
	if strings.TrimSpace(spec.RestartPolicy) != "" {
		args = append(args, "--restart", spec.RestartPolicy)
	}
	for _, port := range spec.Ports {
		args = append(args, "-p", port.String())
	}
	if strings.TrimSpace(spec.Network) != "" {
		args = append(args, "--network", spec.Network)
	}
	args = append(args, spec.Image)

	return combined(ctx, args...)
}

func (manager *Manager) RemoveContainer(ctx context.Context, name string) (string, error) {
	return combined(ctx, "rm", "-f", name)
}

// CopyToContainer uploads a host file into a running container.
func (manager *Manager) CopyToContainer(
	ctx context.Context,
	hostPath string,
	containerName string,
	containerPath string,
) (string, error) {
	return combined(ctx, "cp", hostPath, fmt.Sprintf("%s:%s", containerName, containerPath))
}

func (manager *Manager) ExecInContainer(
	ctx context.Context,
	containerName string,
	command ...string,
) (string, error) {
	return combined(ctx, append([]string{"exec", containerName}, command...)...)
}

// FindContainerNameByLabel returns the first container carrying the label,
// preferring a running one before falling back to stopped containers.
func (manager *Manager) FindContainerNameByLabel(ctx context.Context, label string) (string, error) {
	name, err := manager.findContainerNameByLabel(ctx, true, label)
	if err == nil {
		return name, nil
	}
	return manager.findContainerNameByLabel(ctx, false, label)
}

func (manager *Manager) findContainerNameByLabel(
	ctx context.Context,
	runningOnly bool,
	label string,
) (string, error) {
	args := []string{"ps"}
	if !runningOnly {
		args = append(args, "-a")
	}
	args = append(args, "--filter", "label="+label, "--format", "{{.Names}}")
	names, err := output(ctx, args...)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(strings.TrimSpace(names), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			return name, nil
		}
	}
	return "", fmt.Errorf("container with label %s not found", label)
}

func (manager *Manager) RestartDaemon(ctx context.Context) ([]string, error) {
	transcript := logging.NewTranscript("Restarting Docker daemon...")
	if err := shell.RunTracked(ctx, transcript, "cannot restart docker daemon", "systemctl", "restart", "docker"); err != nil {
		return transcript.Lines(), err
	}
	return transcript.Done("Docker daemon restarted.")
}

func (manager *Manager) ScheduleServerReset(ctx context.Context) ([]string, error) {
	transcript := logging.NewTranscript(
		"Scheduling VPS reset...",
		"Server reboot will start in a few seconds.",
	)
	err := shell.RunTracked(
		ctx,
		transcript,
		"cannot schedule server reboot",
		"sh", "-c", "nohup sh -c 'sleep 3; systemctl reboot' >/tmp/tug-reset.log 2>&1 &",
	)
	if err != nil {
		return transcript.Lines(), err
	}
	return transcript.Done("Reset scheduled.")
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func splitCSV(raw string) []string {
	clean := strings.TrimSpace(raw)
	if clean == "" {
		return []string{}
	}
	parts := strings.Split(clean, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value != "" {
			values = append(values, value)
		}
	}
	return values
}
