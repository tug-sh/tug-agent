package agent

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"tug.sh/pkg/protocol"
	"tug.sh/services/agent/internal/lifecycle"
	"tug.sh/services/agent/internal/shell"
)

const (
	execCommandTimeout    = 3 * time.Minute
	serviceRestartDelay   = 1 * time.Second
	processShutdownGrace  = 2 * time.Second
	disconnectCleanupWait = 1500 * time.Millisecond
	disconnectExitGrace   = 500 * time.Millisecond
)

func (runtime *Runtime) handleExecCommand(request commandRequest) ([]string, error) {
	commandLine, err := require(request.command.Command, "command")
	if err != nil {
		return nil, err
	}
	execCtx, cancel := request.withTimeout(execCommandTimeout)
	defer cancel()

	shellCommand := shell.Custom(execCtx, commandLine)
	combinedOutput, execErr := shellCommand.CombinedOutput()
	output := describeCommandOutput(string(combinedOutput), execErr)

	exitCode := 0
	if shellCommand.ProcessState != nil {
		exitCode = shellCommand.ProcessState.ExitCode()
	}
	request.setPayload(protocol.ExecResult{Output: output, ExitCode: exitCode})
	return []string{output}, execErr
}

func describeCommandOutput(raw string, execErr error) string {
	if strings.TrimSpace(raw) != "" {
		return raw
	}
	if execErr != nil {
		return fmt.Sprintf("Command failed: %v", execErr)
	}
	return "Command executed cleanly (no output)."
}

func (runtime *Runtime) handleSelfUpdate(request commandRequest) ([]string, error) {
	reportProgress := func(downloaded uint64, total uint64, percent int) {
		_ = runtime.emitSignal(request.conn, protocol.EntityCommand, protocol.ActionProgress, protocol.UpdateProgress{
			CommandID:       request.command.CommandID,
			DownloadedBytes: downloaded,
			TotalBytes:      total,
			Percent:         percent,
		})
	}
	if err := runtime.updater.SafeUpdateWithProgress(request.ctx, request.command.BinaryURL, request.command.Version, reportProgress); err != nil {
		return nil, err
	}
	go runtime.restartServiceAfterUpdate()
	target := strings.TrimSpace(request.command.Version)
	if target == "" {
		target = "latest"
	}
	return []string{"Agent binary updated to v" + target + " successfully. Service restarting..."}, nil
}

// restartServiceAfterUpdate hands the process over to systemd once the new
// binary is in place. The delay lets the command result reach the API first.
func (runtime *Runtime) restartServiceAfterUpdate() {
	time.Sleep(serviceRestartDelay)
	if err := exec.Command("systemctl", "restart", "tug-agent.service").Start(); err != nil {
		_ = exec.Command("systemctl", "restart", "tug-agent").Start()
	}
	time.Sleep(processShutdownGrace)
	os.Exit(0)
}

func (runtime *Runtime) handleDisconnect(request commandRequest) ([]string, error) {
	cleanDockerResources := request.command.CleanDockerResources
	go func() {
		time.Sleep(disconnectCleanupWait)
		if err := lifecycle.RunDetachedUninstall(cleanDockerResources); err != nil {
			runtime.log.Error("disconnect uninstall failed: %v", err)
		}
		time.Sleep(disconnectExitGrace)
		os.Exit(0)
	}()
	return []string{"Disconnect acknowledged. Agent process shutting down."}, nil
}
