package agent

import (
	"context"
	"encoding/json"

	"github.com/gorilla/websocket"

	"tug.sh/pkg/protocol"
)

const (
	commandStatusReceived  = protocol.StatusReceived
	commandStatusRunning   = protocol.StatusRunning
	commandStatusSucceeded = protocol.StatusSucceeded
	commandStatusFailed    = protocol.StatusFailed
)

// runCommand executes one dashboard command and reports its lifecycle back.
func (runtime *Runtime) runCommand(ctx context.Context, conn *websocket.Conn, command protocol.Command) {
	if runtime.replayIdempotentCommand(conn, command) {
		return
	}
	// Streaming commands (terminal, log tails) report their own lifecycle, so
	// they are not tracked in the inbox and get no received/running progress.
	tracked := command.CommandID != "" && !protocol.IsStreamingCommand(command.Type)
	if tracked {
		runtime.reportCommandProgress(conn, command.CommandID, commandStatusReceived)
		runtime.commandInbox.markRunning(command.CommandID)
		runtime.reportCommandProgress(conn, command.CommandID, commandStatusRunning)
	}

	var payload json.RawMessage
	logs, execErr := runtime.executeCommand(ctx, conn, command, &payload)
	if command.CommandID != "" {
		runtime.publishCommandResult(conn, command, logs, payload, execErr)
	}
	if execErr != nil {
		runtime.log.Error("command %s (id=%s) failed: %v", command.Type, command.CommandID, execErr)
	} else {
		switch command.Type {
		case protocol.CmdDeploy, protocol.CmdGitDeploy, protocol.CmdContainerAction:
			runtime.publishAllContainerStatuses(ctx, conn)
		}
	}
}

// sendProgress reports an intermediate state. It is a signal, so a delivery
// failure is ignored: the result that follows is the authoritative message and
// it is queued.
func (runtime *Runtime) sendProgress(conn *websocket.Conn, progress protocol.CommandProgress) {
	_ = runtime.emitSignal(conn, protocol.EntityCommand, protocol.ActionProgress, progress)
}

func (runtime *Runtime) reportCommandProgress(conn *websocket.Conn, commandID string, status string) {
	runtime.sendProgress(conn, protocol.CommandProgress{CommandID: commandID, Status: status})
}

func (runtime *Runtime) publishCommandResult(
	conn *websocket.Conn,
	command protocol.Command,
	logs []string,
	payload json.RawMessage,
	execErr error,
) {
	result := protocol.CommandResult{
		CommandID: command.CommandID,
		Success:   execErr == nil,
		Logs:      logs,
		Payload:   payload,
	}
	status := commandStatusSucceeded
	if execErr != nil {
		result.Error = execErr.Error()
		status = commandStatusFailed
	}

	if protocol.IsStreamingCommand(command.Type) {
		// A streaming command's result is only meaningful to whoever is
		// watching right now, so it goes straight out and is not kept.
		_ = runtime.emitSignal(conn, protocol.EntityCommand, protocol.ActionResult, result)
	} else {
		runtime.commandInbox.markResult(result)
		// Queued rather than written: somebody in the dashboard is waiting for
		// this, and a result lost with the socket leaves them waiting forever.
		if err := runtime.emitFact(protocol.EntityCommand, protocol.ActionResult, "result-"+command.CommandID, result); err != nil {
			runtime.log.Error("cannot queue the result of command_id=%s: %v", command.CommandID, err)
		}
	}
	runtime.sendProgress(conn, protocol.CommandProgress{
		CommandID: command.CommandID,
		Status:    status,
		Error:     result.Error,
		Logs:      logs,
		Payload:   payload,
	})
}

// replayIdempotentCommand answers a command the agent already handled, which
// happens when the API retries after a reconnect.
func (runtime *Runtime) replayIdempotentCommand(conn *websocket.Conn, command protocol.Command) bool {
	if runtime.commandInbox == nil || command.CommandID == "" || protocol.IsStreamingCommand(command.Type) {
		return false
	}
	receipt, ok := runtime.commandInbox.get(command.CommandID)
	if !ok {
		return false
	}
	switch receipt.Status {
	case commandStatusSucceeded, commandStatusFailed:
		_ = runtime.emitSignal(conn, protocol.EntityCommand, protocol.ActionResult, receipt.Result)
		runtime.sendProgress(conn, protocol.CommandProgress{
			CommandID: command.CommandID,
			Status:    receipt.Status,
			Error:     receipt.Result.Error,
			Logs:      receipt.Result.Logs,
			Payload:   receipt.Result.Payload,
		})
		return true
	case commandStatusRunning:
		runtime.reportCommandProgress(conn, command.CommandID, commandStatusRunning)
		return true
	default:
		return false
	}
}
