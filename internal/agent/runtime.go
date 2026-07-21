package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"tug.sh/services/agent/internal/config"
)

type Runtime struct {
	config        config.Config
	fileManager   *FileManager
	dockerManager *DockerManager
	updater       *Updater
	writeMu       sync.Mutex

	termMu    sync.Mutex
	terminals map[string]*TerminalSession
}

type nonRetriableError struct {
	err error
}

func (e nonRetriableError) Error() string {
	return e.err.Error()
}

func (e nonRetriableError) Unwrap() error {
	return e.err
}

func markNonRetriable(err error) error {
	if err == nil {
		return nil
	}
	return nonRetriableError{err: err}
}

func isNonRetriable(err error) bool {
	var target nonRetriableError
	return errors.As(err, &target)
}

func (r *Runtime) logReconnectRecoveryHint(reason error) {
	envPath := strings.TrimSpace(r.config.AgentEnvPath)
	if envPath == "" {
		envPath = "/etc/tug/agent.env"
	}
	log.Printf("reconnect disabled: %v", reason)
	log.Printf("recovery steps:")
	log.Printf("1) Verify agent state: sudo tug --status")
	log.Printf("2) Re-initialize token and link: sudo tug --init")
	log.Printf("3) Open generated /connect/<token> URL in dashboard to authorize this host")
	log.Printf("4) Ensure %s contains valid TUG_AGENT_TOKEN", envPath)
}

func NewRuntime(cfg config.Config) (*Runtime, error) {
	return &Runtime{
		config:        cfg,
		fileManager:   NewFileManager(),
		dockerManager: NewDockerManager(),
		updater:       NewUpdater(),
		terminals:     make(map[string]*TerminalSession),
	}, nil
}

func (r *Runtime) debugf(format string, args ...any) {
	if !r.config.Verbose {
		return
	}
	log.Printf("[agent verbose] "+format, args...)
}

func (r *Runtime) Run(ctx context.Context) error {
	if strings.TrimSpace(r.config.AgentToken) == "" || strings.TrimSpace(r.config.ServerID) == "" {
		r.logReconnectRecoveryHint(fmt.Errorf("agent is not initialized (missing or invalid token)"))
		<-ctx.Done()
		return nil
	}

	type retryBurst struct {
		attempts int
		cooldown time.Duration
	}
	retryPlan := []retryBurst{
		{attempts: 5, cooldown: 0},
		{attempts: 5, cooldown: 5 * time.Minute},
		{attempts: 5, cooldown: 30 * time.Minute},
		{attempts: 5, cooldown: 60 * time.Minute},
	}
	const betweenAttemptsDelay = 5 * time.Second
	burstIndex := 0
	attemptInBurst := 0

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		if burstIndex >= len(retryPlan) {
			// Instead of giving up, repeat the last wave indefinitely
			burstIndex = len(retryPlan) - 1
		}

		currentBurst := retryPlan[burstIndex]
		if attemptInBurst == 0 && currentBurst.cooldown > 0 {
			log.Printf(
				"connection retry wave %d/%d starts in %s",
				burstIndex+1,
				len(retryPlan),
				currentBurst.cooldown,
			)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(currentBurst.cooldown):
			}
		}

		attemptInBurst++
		log.Printf(
			"connection attempt %d/%d in wave %d/%d",
			attemptInBurst,
			currentBurst.attempts,
			burstIndex+1,
			len(retryPlan),
		)
		connected, err := r.connectAndServe(ctx)
		if err != nil {
			log.Printf("connection closed: %v", err)
			if isNonRetriable(err) {
				r.logReconnectRecoveryHint(err)
				log.Printf("auth/config error; cooling down for 5 minutes before retrying...")
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(5 * time.Minute):
				}
				// Reset burst to try connecting from scratch
				burstIndex = 0
				attemptInBurst = 0
				continue
			}
		}
		if connected {
			// Reset retries when we had an established socket session.
			burstIndex = 0
			attemptInBurst = 0
			continue
		}
		if attemptInBurst >= currentBurst.attempts {
			burstIndex++
			attemptInBurst = 0
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(betweenAttemptsDelay):
		}
	}
}

func (r *Runtime) connectAndServe(ctx context.Context) (bool, error) {
	const stableConnectionThreshold = 20 * time.Second
	isStableSession := func(startedAt time.Time) bool {
		return time.Since(startedAt) >= stableConnectionThreshold
	}

	if strings.TrimSpace(r.config.ServerID) == "" {
		return false, markNonRetriable(fmt.Errorf("server_id cannot be derived from token; run `tug --init`"))
	}
	if strings.TrimSpace(r.config.AgentToken) == "" {
		return false, markNonRetriable(fmt.Errorf("agent token is missing; run `tug --init`"))
	}
	url := fmt.Sprintf("%s?workspace_id=%s&server_id=%s&token=%s",
		r.config.APIWebSocketURL,
		"",
		r.config.ServerID,
		r.config.AgentToken,
	)
	r.debugf("dial websocket: %s", url)

	conn, response, err := websocket.DefaultDialer.DialContext(ctx, url, http.Header{})
	if err != nil {
		if response != nil && (response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) {
			return false, markNonRetriable(fmt.Errorf("websocket authorization rejected (status %d); run `tug --init`", response.StatusCode))
		}
		return false, fmt.Errorf("websocket dial failed: %w", err)
	}
	defer conn.Close()
	r.debugf("websocket connected")
	sessionStartedAt := time.Now()

	if err := r.sendHandshake(conn); err != nil {
		return isStableSession(sessionStartedAt), err
	}
	r.debugf("initial handshake sent")
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go r.periodicHandshake(sessionCtx, conn, 60*time.Second)

	for {
		_, message, readErr := conn.ReadMessage()
		if readErr != nil {
			return isStableSession(sessionStartedAt), readErr
		}
		var meta struct {
			Type  string `json:"type"`
			Error string `json:"error"`
		}
		if metaErr := json.Unmarshal(message, &meta); metaErr == nil && strings.EqualFold(strings.TrimSpace(meta.Type), "auth_error") {
			details := strings.TrimSpace(meta.Error)
			if details == "" {
				details = "unauthorized agent connection"
			}
			return false, markNonRetriable(fmt.Errorf("websocket authorization rejected: %s; run `tug --init`", details))
		}
		var command inboundCommand
		if err := json.Unmarshal(message, &command); err != nil {
			log.Printf("invalid command payload: %v", err)
			continue
		}
		r.debugf("received command: type=%s", command.Type)
		var payload json.RawMessage
		logs, err := r.executeCommand(ctx, conn, command, &payload)
		if command.CommandID != "" {
			result := outboundCommandResult{
				Type:      "command_result",
				CommandID: command.CommandID,
				Success:   err == nil,
				Logs:      logs,
				Payload:   payload,
			}
			if err != nil {
				result.Error = err.Error()
			}
			if writeErr := r.writeJSON(conn, result); writeErr != nil {
				log.Printf("cannot send command result: %v", writeErr)
			}
		}
		if err != nil {
			log.Printf("command %s failed: %v", command.Type, err)
		}
	}
}

func (r *Runtime) periodicHandshake(ctx context.Context, conn *websocket.Conn, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.sendHandshake(conn); err != nil {
				r.debugf("periodic handshake failed: %v", err)
				return
			}
			r.debugf("periodic handshake sent")
		}
	}
}

func (r *Runtime) sendHandshake(conn *websocket.Conn) error {
	hello, err := r.buildHandshake()
	if err != nil {
		return err
	}

	if err := r.writeJSON(conn, hello); err != nil {
		return fmt.Errorf("cannot write handshake: %w", err)
	}
	r.debugf("handshake payload sent: containers=%d", len(hello.Containers))
	return nil
}

func (r *Runtime) writeJSON(conn *websocket.Conn, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err = conn.WriteMessage(websocket.TextMessage, raw)
	conn.SetWriteDeadline(time.Time{})
	return err
}

type inboundCommand struct {
	Type                 string `json:"type"`
	CommandID            string `json:"command_id"`
	BinaryURL            string `json:"binary_url"`
	Image                string `json:"image"`
	CleanDockerResources bool   `json:"clean_docker_resources"`
	ContainerID          string `json:"container_id"`
	TargetContainerName  string `json:"target_container_name"`
	TargetContainerID    string `json:"target_container_id"`
	TargetPort           int    `json:"target_port"`
	Action               string `json:"action"`
	RemoveVolumes        bool   `json:"remove_volumes"`
	RemoveImage          bool   `json:"remove_image"`
	Domain               string `json:"domain"`
	NetworkName          string `json:"network_name"`
	Content              string `json:"content"`
	Summary              string `json:"summary"`
	ConfigID             string `json:"config_id"`
	ProjectID            string `json:"project_id"`
	RepoURL              string `json:"repo_url"`
	Branch               string `json:"branch"`
	FilePath             string `json:"file_path"`
	FileType             string `json:"file_type"`
	Command              string `json:"command"`
	TerminalID           string `json:"terminal_id"`
	Rows                 uint16 `json:"rows"`
	Cols                 uint16 `json:"cols"`
	Payload              string `json:"payload"`
	Tail                 int    `json:"tail"`
}

type outboundCommandResult struct {
	Type      string          `json:"type"`
	CommandID string          `json:"command_id"`
	Success   bool            `json:"success"`
	Error     string          `json:"error,omitempty"`
	Logs      []string        `json:"logs,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

func (r *Runtime) executeCommand(
	ctx context.Context,
	conn *websocket.Conn,
	command inboundCommand,
	payloadOut *json.RawMessage,
) ([]string, error) {
	switch command.Type {
	case "terminal_start", "terminal_input", "terminal_resize":
		return nil, r.handleTerminalCommand(ctx, conn, command)
	case "git_deploy":
		return r.handleGitDeploy(ctx, command)
	case "fs_list":
		entries, err := r.fileManager.List(command.FilePath)
		if err != nil {
			return nil, err
		}
		type fInfo struct {
			Name  string `json:"name"`
			IsDir bool   `json:"is_dir"`
			Size  int64  `json:"size"`
		}
		var list []fInfo
		for _, e := range entries {
			info, _ := e.Info()
			sz := int64(0)
			if info != nil {
				sz = info.Size()
			}
			list = append(list, fInfo{Name: e.Name(), IsDir: e.IsDir(), Size: sz})
		}
		if raw, err := json.Marshal(list); err == nil {
			*payloadOut = json.RawMessage(raw)
		}
		return []string{"Listed " + command.FilePath}, nil
	case "fs_read":
		data, err := r.fileManager.Read(command.FilePath)
		if err != nil {
			return nil, err
		}
		*payloadOut = json.RawMessage(fmt.Sprintf("%q", string(data)))
		return []string{"Read " + command.FilePath}, nil
	case "fs_write":
		if err := r.fileManager.Write(command.FilePath, []byte(command.Content), 0o640); err != nil {
			return nil, err
		}
		return []string{"Written to " + command.FilePath}, nil
	case "fs_delete":
		if err := r.fileManager.Delete(command.FilePath); err != nil {
			return nil, err
		}
		return []string{"Deleted " + command.FilePath}, nil
	case "self_update":
		return nil, r.updater.SafeUpdate(ctx, command.BinaryURL)
	case "disconnect":
		if err := RunDetachedUninstall(command.CleanDockerResources); err != nil {
			return nil, err
		}
		go func() {
			time.Sleep(500 * time.Millisecond)
			os.Exit(0)
		}()
		return []string{"Disconnect acknowledged. Agent process shutting down."}, nil
	case "container_action":
		if command.ContainerID == "" {
			return nil, fmt.Errorf("container_id is required")
		}
		actionCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		if err := r.dockerManager.ControlContainer(actionCtx, command.ContainerID, command.Action, command.RemoveVolumes, command.RemoveImage); err != nil {
			return nil, err
		}
		if err := r.sendHandshake(conn); err != nil {
			return nil, err
		}
		return []string{fmt.Sprintf("Container %s %s succeeded.", command.ContainerID, command.Action)}, nil
	case "server_action":
		switch command.Action {
		case "restart_docker":
			restartCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			logs, err := r.dockerManager.RestartDockerDaemon(restartCtx)
			if err != nil {
				return logs, err
			}
			return logs, nil
		case "reset_server":
			resetCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			logs, err := r.dockerManager.ScheduleServerReset(resetCtx)
			if err != nil {
				return logs, err
			}
			return logs, nil
		default:
			return nil, fmt.Errorf("unsupported server action %s", command.Action)
		}
	case "network_create":
		if strings.TrimSpace(command.NetworkName) == "" {
			return nil, fmt.Errorf("network_name is required")
		}
		createCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		logs, err := r.dockerManager.CreateNetwork(createCtx, strings.TrimSpace(command.NetworkName))
		if err != nil {
			return logs, err
		}
		if err := r.sendHandshake(conn); err != nil {
			return logs, err
		}
		return logs, nil
	case "network_delete":
		if strings.TrimSpace(command.NetworkName) == "" {
			return nil, fmt.Errorf("network_name is required")
		}
		deleteCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		logs, err := r.dockerManager.DeleteNetwork(deleteCtx, strings.TrimSpace(command.NetworkName))
		if err != nil {
			return logs, err
		}
		if err := r.sendHandshake(conn); err != nil {
			return logs, err
		}
		return logs, nil
	case "save_compose":
		if command.ProjectID == "" {
			return nil, fmt.Errorf("project_id is required")
		}
		if strings.TrimSpace(command.Content) == "" {
			return nil, fmt.Errorf("compose content is required")
		}
		relativePath := filepath.Join("projects", command.ProjectID, "docker-compose.yml")
		if err := r.fileManager.Write(relativePath, []byte(command.Content), 0o640); err != nil {
			return nil, err
		}
		return []string{"Compose file saved."}, nil
	case "container_logs_tail":
		if strings.TrimSpace(command.ContainerID) == "" {
			return nil, fmt.Errorf("container_id is required")
		}
		tail := command.Tail
		if tail <= 0 {
			tail = 200
		}
		if tail > 2000 {
			tail = 2000
		}
		logs, err := r.dockerManager.GetLogsPreview(ctx, strings.TrimSpace(command.ContainerID), tail)
		if err != nil {
			return nil, err
		}
		return logs, nil
	case "containers_snapshot_pull":
		pullCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		containers, err := r.dockerManager.ListContainers(pullCtx)
		if err != nil {
			return nil, err
		}
		type containersSnapshotPayload struct {
			Containers []HandshakeContainer `json:"containers"`
		}
		if raw, marshalErr := json.Marshal(containersSnapshotPayload{Containers: containers}); marshalErr == nil {
			*payloadOut = json.RawMessage(raw)
		}
		return []string{fmt.Sprintf("Containers snapshot size: %d", len(containers))}, nil
	case "deploy":
		if command.ProjectID == "" {
			return nil, fmt.Errorf("project_id is required")
		}
		composePath, err := ResolveSandboxPath(filepath.Join("projects", command.ProjectID, "docker-compose.yml"))
		if err != nil {
			return nil, err
		}
		deployCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		logs, deployErr := r.dockerManager.DeployCompose(deployCtx, composePath, command.Command)
		if deployErr != nil {
			return logs, deployErr
		}
		if err := r.sendHandshake(conn); err != nil {
			return logs, err
		}
		return logs, nil
	case "install_tug_router":
		routerName, nameErr := r.dockerManager.GenerateTugRouterContainerName()
		if nameErr != nil {
			return nil, nameErr
		}
		deployCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		logs, deployErr := r.dockerManager.InstallTugRouter(
			deployCtx,
			routerName,
			strings.TrimSpace(command.Image),
		)
		if deployErr != nil {
			return logs, deployErr
		}
		if err := r.sendHandshake(conn); err != nil {
			return logs, err
		}
		return logs, nil
	case "configure_tug_router_route":
		if strings.TrimSpace(command.Domain) == "" {
			return nil, fmt.Errorf("domain is required")
		}
		if strings.TrimSpace(command.TargetContainerName) == "" {
			return nil, fmt.Errorf("target_container_name is required")
		}
		if command.TargetPort <= 0 || command.TargetPort > 65535 {
			return nil, fmt.Errorf("target_port must be between 1 and 65535")
		}
		routerName, resolveErr := r.dockerManager.ResolveTugRouterContainerName(ctx)
		if resolveErr != nil {
			return nil, fmt.Errorf("tug-router container is not installed")
		}
		configureCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		logs, configureErr := r.dockerManager.ConfigureTugRouterRoute(
			configureCtx,
			routerName,
			strings.TrimSpace(command.Domain),
			strings.TrimSpace(command.TargetContainerID),
			strings.TrimSpace(command.TargetContainerName),
			command.TargetPort,
		)
		if configureErr != nil {
			return logs, configureErr
		}
		return logs, nil
	case "list_tug_router_routes":
		routerName, resolveErr := r.dockerManager.ResolveTugRouterContainerName(ctx)
		if resolveErr != nil {
			return nil, fmt.Errorf("tug-router container is not installed")
		}
		routes, listErr := r.dockerManager.ListTugRouterRoutes(routerName)
		if listErr != nil {
			return nil, listErr
		}
		type routesPayload struct {
			Routes any `json:"routes"`
		}
		if raw, marshalErr := json.Marshal(routesPayload{Routes: routes}); marshalErr == nil {
			*payloadOut = json.RawMessage(raw)
		}
		return []string{fmt.Sprintf("Loaded %d route(s).", len(routes))}, nil
	case "remove_tug_router_route":
		if strings.TrimSpace(command.Domain) == "" {
			return nil, fmt.Errorf("domain is required")
		}
		routerName, resolveErr := r.dockerManager.ResolveTugRouterContainerName(ctx)
		if resolveErr != nil {
			return nil, fmt.Errorf("tug-router container is not installed")
		}
		removeCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		logs, removeErr := r.dockerManager.DeleteTugRouterRoute(
			removeCtx,
			routerName,
			strings.TrimSpace(command.Domain),
		)
		if removeErr != nil {
			return logs, removeErr
		}
		return logs, nil
	case "check_host_path":
		if command.FilePath == "" {
			return nil, fmt.Errorf("file_path is required")
		}
		info, err := os.Stat(command.FilePath)
		if err != nil {
			if os.IsNotExist(err) {
				type pathInfo struct {
					Exists bool `json:"exists"`
					IsDir  bool `json:"is_dir"`
				}
				raw, _ := json.Marshal(pathInfo{Exists: false, IsDir: false})
				*payloadOut = json.RawMessage(raw)
				return []string{"Path does not exist: " + command.FilePath}, nil
			}
			return nil, fmt.Errorf("path access error: %w", err)
		}
		type pathInfo struct {
			Exists bool `json:"exists"`
			IsDir  bool `json:"is_dir"`
		}
		raw, _ := json.Marshal(pathInfo{Exists: true, IsDir: info.IsDir()})
		*payloadOut = json.RawMessage(raw)
		return []string{"Checked path: " + command.FilePath}, nil
	case "get_container_mounts":
		if command.ContainerID == "" {
			return nil, fmt.Errorf("container_id is required")
		}
		out, err := exec.CommandContext(ctx, "docker", "inspect", "--format={{json .Mounts}}", command.ContainerID).Output()
		if err != nil {
			return nil, fmt.Errorf("docker inspect failed: %w", err)
		}
		*payloadOut = json.RawMessage(out)
		return []string{"Fetched container mounts for " + command.ContainerID}, nil
	default:
		return nil, nil
	}
}

func (r *Runtime) buildHandshake() (Handshake, error) {
	publicIP, _ := fetchPublicIP()
	localIP := detectLocalIP()
	dockerVersion, _ := detectDockerVersion()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	containers, _ := r.dockerManager.ListContainers(ctx)
	networks, _ := r.dockerManager.ListNetworks(ctx)
	hostName, _ := os.Hostname()
	totalRAMBytes, err := detectTotalRAMBytes()
	if err != nil {
		return Handshake{}, err
	}

	diskFreeBytes, err := detectDiskFree(GetDataDir())
	if err != nil {
		return Handshake{}, err
	}
	r.debugf(
		"build handshake snapshot: host=%s local_ip=%s public_ip=%s docker=%s containers=%d",
		hostName,
		localIP,
		publicIP,
		dockerVersion,
		len(containers),
	)

	return Handshake{
		Type:          "handshake",
		ServerID:      r.config.ServerID,
		WorkspaceID:   r.config.WorkspaceID,
		HostName:      hostName,
		AgentVersion:  r.config.AgentVersion,
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		CPUCores:      runtime.NumCPU(),
		RAMBytes:      totalRAMBytes,
		DiskFreeBytes: diskFreeBytes,
		LocalIP:       localIP,
		PublicIP:      publicIP,
		DockerVersion: dockerVersion,
		Networks:      networks,
		Containers:    containers,
	}, nil
}

func detectDockerVersion() (string, error) {
	cmd := exec.Command("docker", "version", "--format", "{{.Server.Version}}")
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func detectLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() || ipNet.IP.To4() == nil {
			continue
		}
		return ipNet.IP.String()
	}
	return ""
}

func fetchPublicIP() (string, error) {
	req, err := http.NewRequest(http.MethodGet, "https://api.ipify.org", nil)
	if err != nil {
		return "", err
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var body []byte
	body, err = ioReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}
