package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"tug.sh/services/agent/internal/protocol"
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

// commandHandlers maps every command type dispatched by the API to its handler.
// Unknown types resolve to no handler and are silently ignored, which keeps an
// older agent compatible with a newer API.
var commandHandlers = map[string]commandHandler{
	"get_docker_disk_usage":      (*Runtime).handleDockerDiskUsage,
	"prune":                      (*Runtime).handleDockerPrune,
	"run_docker_prune":           (*Runtime).handleDockerPrune,
	"terminal_start":             (*Runtime).handleTerminal,
	"terminal_input":             (*Runtime).handleTerminal,
	"terminal_resize":            (*Runtime).handleTerminal,
	"terminal_stop":              (*Runtime).handleTerminal,
	"run_cron_task":              (*Runtime).handleExecCommand,
	"exec_command":               (*Runtime).handleExecCommand,
	"git_deploy":                 (*Runtime).handleGitDeployCommand,
	"fs_list":                    (*Runtime).handleFileList,
	"fs_read":                    (*Runtime).handleFileRead,
	"fs_write":                   (*Runtime).handleFileWrite,
	"fs_delete":                  (*Runtime).handleFileDelete,
	"self_update":                (*Runtime).handleSelfUpdate,
	"disconnect":                 (*Runtime).handleDisconnect,
	"container_action":           (*Runtime).handleContainerAction,
	"server_action":              (*Runtime).handleServerAction,
	"network_create":             (*Runtime).handleNetworkCreate,
	"network_delete":             (*Runtime).handleNetworkDelete,
	"save_compose":               (*Runtime).handleSaveCompose,
	"cron_schedules_apply":       (*Runtime).handleCronSchedulesApply,
	"cron_schedules_pull":        (*Runtime).handleCronSchedulesPull,
	"container_logs_tail":        (*Runtime).handleContainerLogsTail,
	"containers_snapshot_pull":   (*Runtime).handleContainersSnapshotPull,
	"deploy":                     (*Runtime).handleDeploy,
	"install_tug_router":         (*Runtime).handleInstallTugRouter,
	"configure_tug_router_route": (*Runtime).handleConfigureTugRouterRoute,
	"list_tug_router_routes":     (*Runtime).handleListTugRouterRoutes,
	"remove_tug_router_route":    (*Runtime).handleRemoveTugRouterRoute,
	"check_host_path":            (*Runtime).handleCheckHostPath,
	"get_container_mounts":       (*Runtime).handleContainerMounts,
	"container_inspect":          (*Runtime).handleContainerInspect,
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
