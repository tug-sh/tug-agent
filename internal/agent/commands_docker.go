package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tug.sh/pkg/protocol"
	"tug.sh/services/agent/internal/docker"
	"tug.sh/services/agent/internal/sandbox"
)

const (
	containerActionTimeout    = 60 * time.Second
	networkActionTimeout      = 30 * time.Second
	deployTimeout             = 2 * time.Minute
	snapshotPullTimeout       = 20 * time.Second
	serverResetTimeout        = 15 * time.Second
	migrationPreflightTimeout = 20 * time.Second
	defaultLogsTailLines      = 200
	maxLogsTailLines          = 2000
)

func (runtime *Runtime) handleDockerDiskUsage(request commandRequest) ([]string, error) {
	report, err := runtime.dockerManager.DiskUsage(request.ctx)
	if err != nil {
		return nil, err
	}
	request.setPayload(report)
	return []string{"Fetched Docker disk usage report successfully."}, nil
}

func (runtime *Runtime) handleDockerPrune(request commandRequest) ([]string, error) {
	return runtime.dockerManager.Prune(request.ctx)
}

func (runtime *Runtime) handleContainerAction(request commandRequest) ([]string, error) {
	containerID, err := request.requireContainerID()
	if err != nil {
		return nil, err
	}
	actionCtx, cancel := request.withTimeout(containerActionTimeout)
	defer cancel()

	err = runtime.dockerManager.ControlContainer(
		actionCtx,
		containerID,
		request.command.Action,
		request.command.RemoveVolumes,
		request.command.RemoveImage,
	)
	if err != nil {
		return nil, err
	}
	if handshakeErr := runtime.sendSnapshot(); handshakeErr != nil {
		return nil, handshakeErr
	}
	runtime.publishContainerStatus(request.ctx, request.conn, containerID)
	return []string{fmt.Sprintf("Container %s %s succeeded.", containerID, request.command.Action)}, nil
}

func (runtime *Runtime) handleServerAction(request commandRequest) ([]string, error) {
	switch strings.TrimSpace(request.command.Action) {
	case "restart_docker":
		actionCtx, cancel := request.withTimeout(containerActionTimeout)
		defer cancel()
		return runtime.dockerManager.RestartDaemon(actionCtx)
	case "reset_server":
		actionCtx, cancel := request.withTimeout(serverResetTimeout)
		defer cancel()
		return runtime.dockerManager.ScheduleServerReset(actionCtx)
	default:
		return nil, fmt.Errorf("unsupported server action %s", request.command.Action)
	}
}

func (runtime *Runtime) handleNetworkCreate(request commandRequest) ([]string, error) {
	return runtime.changeNetwork(request, runtime.dockerManager.CreateNetwork)
}

func (runtime *Runtime) handleNetworkDelete(request commandRequest) ([]string, error) {
	return runtime.changeNetwork(request, runtime.dockerManager.DeleteNetwork)
}

// changeNetwork applies a network change and re-syncs the dashboard, which is
// the shared shape of both network commands.
func (runtime *Runtime) changeNetwork(
	request commandRequest,
	apply func(ctx context.Context, name string) ([]string, error),
) ([]string, error) {
	networkName, err := require(request.command.NetworkName, "network_name")
	if err != nil {
		return nil, err
	}
	changeCtx, cancel := request.withTimeout(networkActionTimeout)
	defer cancel()

	logs, applyErr := apply(changeCtx, networkName)
	if applyErr != nil {
		return logs, applyErr
	}
	return logs, runtime.sendSnapshot()
}

func (runtime *Runtime) handleDeploy(request commandRequest) ([]string, error) {
	projectID, err := request.requireProjectID()
	if err != nil {
		return nil, err
	}
	composePath, err := sandbox.ResolvePath(filepath.Join("projects", projectID, "docker-compose.yml"))
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.command.ComposeContent) != "" {
		if mkdirErr := os.MkdirAll(filepath.Dir(composePath), 0o755); mkdirErr == nil {
			_ = os.WriteFile(composePath, []byte(request.command.ComposeContent), 0o644)
		}
	}
	deployCtx, cancel := request.withTimeout(deployTimeout)
	defer cancel()

	logs, deployErr := runtime.dockerManager.DeployCompose(deployCtx, composePath, request.command.Command)
	writeDeploymentLog(projectID, request.command.CommandID, logs, deployErr)
	if deployErr != nil {
		return logs, deployErr
	}
	if handshakeErr := runtime.sendSnapshot(); handshakeErr != nil {
		return logs, handshakeErr
	}
	runtime.publishAllContainerStatuses(request.ctx, request.conn)
	return logs, nil
}

func (runtime *Runtime) handleContainerLogsTail(request commandRequest) ([]string, error) {
	containerID, err := request.requireContainerID()
	if err != nil {
		return nil, err
	}
	return runtime.dockerManager.GetLogsPreview(request.ctx, containerID, clampTailLines(request.command.Tail))
}

func clampTailLines(requested int) int {
	if requested <= 0 {
		return defaultLogsTailLines
	}
	if requested > maxLogsTailLines {
		return maxLogsTailLines
	}
	return requested
}

func (runtime *Runtime) handleContainersSnapshotPull(request commandRequest) ([]string, error) {
	pullCtx, cancel := request.withTimeout(snapshotPullTimeout)
	defer cancel()

	containers, err := runtime.dockerManager.ListContainers(pullCtx)
	if err != nil {
		return nil, err
	}
	request.setPayload(protocol.ContainersSnapshot{Containers: containers})
	return []string{fmt.Sprintf("Containers snapshot size: %d", len(containers))}, nil
}

func (runtime *Runtime) handleContainerMounts(request commandRequest) ([]string, error) {
	containerID, err := request.requireContainerID()
	if err != nil {
		return nil, err
	}
	raw, err := runtime.dockerManager.Inspect(request.ctx, containerID, "--format={{json .Mounts}}")
	if err != nil {
		return nil, err
	}
	*request.payload = raw
	return []string{"Fetched container mounts for " + containerID}, nil
}

// handleContainerInspect returns the effective configuration of what actually
// runs on the VPS. It is the authoritative source for the Docker config wizard
// (read-first), so generated compose never silently overwrites live settings.
func (runtime *Runtime) handleContainerInspect(request commandRequest) ([]string, error) {
	containerID, err := request.requireContainerID()
	if err != nil {
		return nil, err
	}
	raw, err := runtime.dockerManager.Inspect(request.ctx, containerID)
	if err != nil {
		return nil, err
	}
	// docker inspect returns a JSON array; forward the single element so the
	// client receives one object.
	var inspected []json.RawMessage
	if unmarshalErr := json.Unmarshal(raw, &inspected); unmarshalErr == nil && len(inspected) > 0 {
		*request.payload = inspected[0]
	} else {
		*request.payload = raw
	}
	return []string{"Inspected container " + containerID}, nil
}

func (runtime *Runtime) handlePrepareMigrationTarget(request commandRequest) ([]string, error) {
	if err := docker.PrepareMigrationTargetKey(request.ctx, request.command.EphemeralKey); err != nil {
		return nil, err
	}
	return []string{"Prepared target VPS migration SSH key successfully."}, nil
}

// handleMigrationPreflight answers, from the source machine, whether the target
// can actually be reached before any state-changing work begins. Direct
// migration needs a routable path to the target's SSH port; a target behind NAT
// with no reachable address fails here rather than half-way through the transfer.
func (runtime *Runtime) handleMigrationPreflight(request commandRequest) ([]string, error) {
	migCtx, cancel := request.withTimeout(migrationPreflightTimeout)
	defer cancel()
	return docker.CheckMigrationReachable(migCtx, request.command.TargetIP, request.command.TargetSSHPort)
}

func (runtime *Runtime) handleMigrateContainerSource(request commandRequest) ([]string, error) {
	containerID, err := request.requireContainerID()
	if err != nil {
		return nil, err
	}
	migCtx, cancel := request.withTimeout(5 * time.Minute)
	defer cancel()

	// Each step is streamed as progress so the task history fills in live, and
	// the same lines are returned so they survive as the final result even if
	// the browser was not watching. On failure the collected steps go back too,
	// so the log shows how far it got before the error.
	var logs []string
	report := func(line string) {
		logs = append(logs, line)
		runtime.sendProgress(request.conn, protocol.CommandProgress{
			CommandID: request.command.CommandID,
			Status:    protocol.StatusRunning,
			Logs:      []string{line},
		})
	}

	err = runtime.dockerManager.MigrateContainerToTarget(
		migCtx,
		containerID,
		request.command.TargetIP,
		request.command.TargetSSHPort,
		request.command.EphemeralKey,
		request.command.MoveMode,
		report,
	)
	if err != nil {
		return logs, err
	}

	_ = runtime.sendSnapshot()
	logs = append(logs, fmt.Sprintf("Migrated container %s to target %s successfully.", containerID, request.command.TargetIP))
	return logs, nil
}
