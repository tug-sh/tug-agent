package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"tug.sh/pkg/protocol"
)

// commandRequest carries a decoded command together with the transport handles
// a handler may need: the websocket for streaming progress and the payload slot
// for the structured result returned to the dashboard.
type commandRequest struct {
	ctx     context.Context
	conn    *websocket.Conn
	command protocol.Command
	payload *json.RawMessage
}

// setPayload attaches the structured result of a command. Marshal failures are
// ignored on purpose, because the textual logs stay the authoritative result.
func (request commandRequest) setPayload(value any) {
	if request.payload == nil {
		return
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return
	}
	*request.payload = json.RawMessage(raw)
}

func (request commandRequest) containerID() string {
	return strings.TrimSpace(request.command.ContainerID)
}

func (request commandRequest) domain() string {
	return strings.TrimSpace(request.command.Domain)
}

// withTimeout bounds a single command, so one stuck docker call cannot occupy a
// handler for the rest of the session.
func (request commandRequest) withTimeout(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(request.ctx, timeout)
}

// require returns the trimmed value or the error the dashboard displays when a
// mandatory command field is missing.
func require(value string, field string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	return trimmed, nil
}

func (request commandRequest) requireContainerID() (string, error) {
	return require(request.command.ContainerID, "container_id")
}

func (request commandRequest) requireProjectID() (string, error) {
	return require(request.command.ProjectID, "project_id")
}

func (request commandRequest) requireDomain() (string, error) {
	return require(request.command.Domain, "domain")
}

type commandHandler func(runtime *Runtime, request commandRequest) ([]string, error)

// commandHandlers maps every command type the API can dispatch to its handler.
//
// The keys are the shared constants rather than string literals. They used to
// be literals, and two of them ("prune", "run_docker_prune") answered a command
// the API never sent while the one it did send, docker_prune, fell through to
// "unknown type" and was silently ignored. A test asserts this map covers
// protocol.AllCommands, so the next such divergence fails the build.
var commandHandlers = map[string]commandHandler{
	protocol.CmdDockerDiskUsage:      (*Runtime).handleDockerDiskUsage,
	protocol.CmdDockerPrune:          (*Runtime).handleDockerPrune,
	protocol.CmdTerminalStart:        (*Runtime).handleTerminal,
	protocol.CmdTerminalInput:        (*Runtime).handleTerminal,
	protocol.CmdTerminalResize:       (*Runtime).handleTerminal,
	protocol.CmdTerminalStop:         (*Runtime).handleTerminal,
	protocol.CmdRunCronTask:          (*Runtime).handleExecCommand,
	protocol.CmdExecCommand:          (*Runtime).handleExecCommand,
	protocol.CmdGitDeploy:            (*Runtime).handleGitDeployCommand,
	protocol.CmdFileList:             (*Runtime).handleFileList,
	protocol.CmdFileRead:             (*Runtime).handleFileRead,
	protocol.CmdFileWrite:            (*Runtime).handleFileWrite,
	protocol.CmdFileDelete:           (*Runtime).handleFileDelete,
	protocol.CmdSelfUpdate:           (*Runtime).handleSelfUpdate,
	protocol.CmdDisconnect:           (*Runtime).handleDisconnect,
	protocol.CmdContainerAction:      (*Runtime).handleContainerAction,
	protocol.CmdServerAction:         (*Runtime).handleServerAction,
	protocol.CmdNetworkCreate:        (*Runtime).handleNetworkCreate,
	protocol.CmdNetworkDelete:        (*Runtime).handleNetworkDelete,
	protocol.CmdSaveCompose:          (*Runtime).handleSaveCompose,
	protocol.CmdCronSchedulesApply:   (*Runtime).handleCronSchedulesApply,
	protocol.CmdCronSchedulesPull:    (*Runtime).handleCronSchedulesPull,
	protocol.CmdContainerLogsTail:    (*Runtime).handleContainerLogsTail,
	protocol.CmdContainersSnapshot:   (*Runtime).handleContainersSnapshotPull,
	protocol.CmdDeploy:               (*Runtime).handleDeploy,
	protocol.CmdRouterInstall:        (*Runtime).handleInstallTugRouter,
	protocol.CmdRouterRouteConfigure: (*Runtime).handleConfigureTugRouterRoute,
	protocol.CmdRouterRouteList:      (*Runtime).handleListTugRouterRoutes,
	protocol.CmdRouterRouteRemove:    (*Runtime).handleRemoveTugRouterRoute,
	protocol.CmdCheckHostPath:        (*Runtime).handleCheckHostPath,
	protocol.CmdContainerMounts:      (*Runtime).handleContainerMounts,
	protocol.CmdContainerInspect:     (*Runtime).handleContainerInspect,
}

func (runtime *Runtime) executeCommand(
	ctx context.Context,
	conn *websocket.Conn,
	command protocol.Command,
	payloadOut *json.RawMessage,
) ([]string, error) {
	handler, known := commandHandlers[strings.TrimSpace(command.Type)]
	if !known {
		return nil, nil
	}
	return handler(runtime, commandRequest{
		ctx:     ctx,
		conn:    conn,
		command: command,
		payload: payloadOut,
	})
}

func (runtime *Runtime) handleTerminal(request commandRequest) ([]string, error) {
	return nil, runtime.handleTerminalCommand(request.ctx, request.conn, request.command)
}

func (runtime *Runtime) handleGitDeployCommand(request commandRequest) ([]string, error) {
	return runtime.handleGitDeploy(request.ctx, request.command)
}
