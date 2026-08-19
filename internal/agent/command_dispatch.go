package agent

import (
	"context"
	"encoding/json"

	"github.com/gorilla/websocket"

	"tug.sh/services/agent/internal/protocol"
)

const (
	commandStatusReceived  = "received"
	commandStatusRunning   = "running"
	commandStatusSucceeded = "succeeded"
	commandStatusFailed    = "failed"
)

// runCommand executes one dashboard command and reports its lifecycle back.
func (runtime *Runtime) runCommand(ctx context.Context, conn *websocket.Conn, command protocol.Command) {
	if runtime.replayIdempotentCommand(conn, command) {
		return
	}
	// Streaming commands (terminal, log tails) report their own lifecycle, so
	// they are not tracked in the inbox and get no received/running progress.
	tracked := command.CommandID != "" && !isStreamCommandType(command.Type)
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
	}
}

// sendProgress stamps a progress frame with the fields every frame carries and
// puts it on the wire. Delivery failures are ignored: the authoritative result
// is replayed from the inbox after a reconnect.
func (runtime *Runtime) sendProgress(conn *websocket.Conn, progress protocol.CommandProgress) {
	progress.Type = "command_progress"
	progress.ServerID = runtime.config.ServerID
	_ = runtime.writeJSON(conn, progress)
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
		Type:      "command_result",
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
	if !isStreamCommandType(command.Type) {
		runtime.commandInbox.markResult(result)
	}
	if writeErr := runtime.writeJSON(conn, result); writeErr != nil {
		runtime.log.Error("cannot send command result for command_id=%s: %v", command.CommandID, writeErr)
	}
	runtime.sendProgress(conn, protocol.CommandProgress{
		CommandID: command.CommandID,
		Status:    status,
		Error:     result.Error,
		Logs:      logs,
		Payload:   payload,
	})
}

// isStreamCommandType marks commands that push their own output frames and
// therefore must not be replayed from the inbox.
func isStreamCommandType(commandType string) bool {
	switch commandType {
	case "terminal_start", "terminal_input", "terminal_resize", "terminal_stop", "container_logs_tail":
		return true
	default:
		return false
	}
}

// replayIdempotentCommand answers a command the agent already handled, which
// happens when the API retries after a reconnect.
func (runtime *Runtime) replayIdempotentCommand(conn *websocket.Conn, command protocol.Command) bool {
	if runtime.commandInbox == nil || command.CommandID == "" || isStreamCommandType(command.Type) {
		return false
	}
	receipt, ok := runtime.commandInbox.get(command.CommandID)
	if !ok {
		return false
	}
	switch receipt.Status {
	case commandStatusSucceeded, commandStatusFailed:
		_ = runtime.writeJSON(conn, receipt.Result)
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
