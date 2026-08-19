package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	websocket "github.com/gorilla/websocket"
	"gopkg.in/natefinch/lumberjack.v2"

	"tug.sh/services/agent/internal/agent"
	"tug.sh/services/agent/internal/config"
	"tug.sh/services/agent/internal/lifecycle"
	"tug.sh/services/agent/internal/logging"
	"tug.sh/services/agent/internal/sandbox"
)

func main() {
	args := os.Args[1:]
	if hasToken(args, "--test-mode") {
		fmt.Println("health-check: ok")
		return
	}

	if err := loadAgentEnvFile(); err != nil {
		logging.Fatal("failed to load environment file: %v", err)
	}

	cfg := config.Load()
	logging.SetDefault(agent.LoggerForConfig(cfg))
	command := parseCommand(args)
	if handler, isUserCommand := cliCommands()[command]; isUserCommand {
		if err := handler.run(cfg, args); err != nil {
			logging.Fatal("%s failed: %v", command, err)
		}
		if handler.success != "" {
			fmt.Println(handler.success)
		}
		return
	}

	switch command {
	case "run", "daemon", "run-service", "service":
	case "":
		if !(isSystemdService() || (!isTerminal(os.Stdout) && !isTerminal(os.Stdin))) {
			printHelp(cfg.AgentVersion)
			return
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", command)
		printHelp(cfg.AgentVersion)
		os.Exit(2)
	}

	logPath := filepath.Join(sandbox.DataDir(), "logs", "agent.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err == nil {
		log.SetOutput(io.MultiWriter(os.Stdout, &lumberjack.Logger{
			Filename:   logPath,
			MaxSize:    10, // megabytes
			MaxBackups: 3,
			MaxAge:     28, // days
			Compress:   true,
		}))
	}

	releaseLock, err := acquireSingleInstanceLock()
	if err != nil {
		logging.Fatal("agent is already running: %v", err)
	}
	defer releaseLock()
	logging.Debug(
		"agent startup: server_id=%s ws_url=%s env_path=%s profile=%s heartbeat=%s self_heal=%s reconnect_base=%s reconnect_max=%s jitter_pct=%d",
		cfg.ServerID,
		cfg.APIWebSocketURL,
		cfg.AgentEnvPath,
		cfg.TrafficProfile,
		cfg.HeartbeatInterval,
		cfg.SelfHealInterval,
		cfg.ReconnectBaseDelay,
		cfg.ReconnectMaxDelay,
		cfg.ReconnectJitterPct,
	)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	runtimeAgent, err := agent.NewRuntime(cfg)
	if err != nil {
		logging.Fatal("failed to create runtime: %v", err)
	}

	if err := runtimeAgent.Run(ctx); err != nil {
		logging.Fatal("agent run failed: %v", err)
	}
}

// cliCommand is one command a user runs from the shell. Failures are reported
// the same way for all of them, so a handler only returns an error.
type cliCommand struct {
	run     func(cfg config.Config, args []string) error
	success string
}

// cliCommands lists everything that runs and exits, as opposed to the daemon
// commands handled by main.
func cliCommands() map[string]cliCommand {
	return map[string]cliCommand{
		"help": {run: func(cfg config.Config, _ []string) error {
			printHelp(cfg.AgentVersion)
			return nil
		}},
		"version": {run: func(cfg config.Config, _ []string) error {
			fmt.Printf("tug-agent v%s\n", cfg.AgentVersion)
			return nil
		}},
		"update": {
			run:     func(cfg config.Config, _ []string) error { return runUpdate(cfg) },
			success: "Agent updated successfully and restarted.",
		},
		"init":   {run: runInit},
		"start":  {run: func(cfg config.Config, _ []string) error { return runStart(cfg) }},
		"status": {run: func(config.Config, []string) error { return runStatus() }},
		"stop": {
			run:     func(config.Config, []string) error { return stopAgentService() },
			success: "Agent stopped.",
		},
		"restart": {run: func(cfg config.Config, _ []string) error { return runRestart(cfg) }},
		"logs": {run: func(_ config.Config, args []string) error {
			return runLogs(parseLogsLimit(args))
		}},
		"disconnect": {
			run:     func(cfg config.Config, _ []string) error { return runDisconnect(cfg) },
			success: "Agent disconnected from dashboard. Run `tug init` to reconnect.",
		},
		"remove": {
			run:     func(config.Config, []string) error { return lifecycle.RunDetachedUninstall(false) },
			success: "Agent uninstall started in background.",
		},
	}
}

// runDisconnect clears the pairing even when the service cannot be stopped,
// so a broken unit never leaves the agent linked to the dashboard.
func runDisconnect(cfg config.Config) error {
	if err := stopAgentService(); err != nil {
		logging.Warn("cannot stop service automatically: %v", err)
	}
	return clearAgentConnectionState(cfg)
}

// parseCommand reads the positional subcommand. Leading flags belong to the
// daemon invocation, so anything starting with "-" is not a command.
func parseCommand(args []string) string {
	if len(args) == 0 {
		return ""
	}
	first := strings.TrimSpace(args[0])
	if strings.HasPrefix(first, "-") {
		return ""
	}
	return first
}

func hasToken(args []string, token string) bool {
	for _, arg := range args {
		if strings.TrimSpace(arg) == token {
			return true
		}
	}
	return false
}

func loadAgentEnvFile() error {
	defaultPath := "/etc/tug/agent.env"
	candidates := make([]string, 0, 3)
	if configuredPath := strings.TrimSpace(os.Getenv("TUG_AGENT_ENV_PATH")); configuredPath != "" {
		candidates = append(candidates, configuredPath)
	}
	candidates = append(candidates, defaultPath, "./agent.env")

	path := ""
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if _, exists := seen[candidate]; exists {
			continue
		}
		seen[candidate] = struct{}{}
		if _, statErr := os.Stat(candidate); statErr == nil {
			path = candidate
			break
		}
	}
	if path == "" {
		if configuredPath := strings.TrimSpace(os.Getenv("TUG_AGENT_ENV_PATH")); configuredPath != "" {
			path = configuredPath
		} else {
			path = defaultPath
		}
	}
	_ = os.Setenv("TUG_AGENT_ENV_PATH", path)

	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("cannot open %s: %w", path, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if key == "" {
			continue
		}
		if os.Getenv(key) != "" {
			continue
		}
		if setErr := os.Setenv(key, value); setErr != nil {
			return fmt.Errorf("cannot set %s from %s: %w", key, path, setErr)
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("cannot read %s: %w", path, err)
	}

	return nil
}

func checkAPIConnection(cfg config.Config) string {
	wsURL := strings.TrimSpace(cfg.APIWebSocketURL)
	if wsURL == "" {
		return "disconnected (ws_url not set)"
	}
	serverID := strings.TrimSpace(cfg.ServerID)
	token := strings.TrimSpace(cfg.AgentToken)
	if serverID == "" || token == "" {
		return "disconnected (not initialized)"
	}

	dialURL := fmt.Sprintf("%s?server_id=%s", wsURL, url.QueryEscape(serverID))
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+token)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, dialURL, headers)
	if err != nil {
		if resp != nil {
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusBadRequest {
				return fmt.Sprintf("disconnected (auth rejected: HTTP %d)", resp.StatusCode)
			}
			return fmt.Sprintf("disconnected (HTTP %d)", resp.StatusCode)
		}
		return fmt.Sprintf("disconnected (unreachable: %v)", err)
	}
	conn.Close()
	return "connected"
}

func runStatus() error {
	if err := loadAgentEnvFile(); err != nil {
		return err
	}
	cfg := config.Load()
	initialized := strings.TrimSpace(cfg.ServerID) != "" && strings.TrimSpace(cfg.AgentToken) != ""

	serviceState := "unknown"
	if output, err := exec.Command("systemctl", "is-active", "tug-agent.service").CombinedOutput(); err == nil {
		serviceState = strings.TrimSpace(string(output))
	} else {
		serviceState = "inactive"
	}

	tokenPreview := "(not set)"
	if strings.TrimSpace(cfg.AgentToken) != "" {
		tokenPreview = cfg.AgentToken
		if len(tokenPreview) > 12 {
			tokenPreview = tokenPreview[:12] + "..."
		}
	}

	fmt.Println("tug agent status")
	fmt.Println("----------------")
	fmt.Printf("version: v%s\n", cfg.AgentVersion)
	fmt.Printf("service: %s\n", serviceState)
	fmt.Printf("initialized: %t\n", initialized)
	fmt.Printf("api_connection: %s\n", checkAPIConnection(cfg))
	fmt.Printf("server_id: %s\n", fallbackValue(cfg.ServerID, "(not set)"))
	fmt.Printf("agent_token: %s\n", tokenPreview)
	fmt.Printf("ws_url: %s\n", fallbackValue(cfg.APIWebSocketURL, "(not set)"))
	fmt.Printf("env_path: %s\n", fallbackValue(cfg.AgentEnvPath, "(not set)"))
	return nil
}

const defaultAgentLogLines = 100
const maxAgentLogLines = 10000

func agentLogPath() string {
	return filepath.Join(sandbox.DataDir(), "logs", "agent.log")
}

func parseLogsLimit(args []string) int {
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if arg == "" || arg == "logs" {
			continue
		}
		if n, err := strconv.Atoi(arg); err == nil {
			return clampLogLimit(n)
		}
	}
	return defaultAgentLogLines
}

func clampLogLimit(n int) int {
	if n < 1 {
		return defaultAgentLogLines
	}
	if n > maxAgentLogLines {
		return maxAgentLogLines
	}
	return n
}

func tailFileLines(path string, limit int) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	if size == 0 {
		return []string{}, nil
	}

	const chunkSize = 8192
	var collected []byte
	offset := size
	newlines := 0
	for offset > 0 && newlines <= limit {
		readSize := int64(chunkSize)
		if readSize > offset {
			readSize = offset
		}
		offset -= readSize
		buf := make([]byte, readSize)
		if _, err := file.ReadAt(buf, offset); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		collected = append(buf, collected...)
		newlines = bytes.Count(collected, []byte{'\n'})
	}

	text := strings.ReplaceAll(string(collected), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return lines, nil
}

func runLogs(limit int) error {
	path := agentLogPath()
	lines, err := tailFileLines(path, limit)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("log file not found: %s", path)
		}
		return err
	}
	if len(lines) == 0 {
		fmt.Printf("No log lines in %s\n", path)
		return nil
	}
	for _, line := range lines {
		fmt.Println(line)
	}
	return nil
}

func runRestart(cfg config.Config) error {
	initialized := strings.TrimSpace(cfg.ServerID) != "" && strings.TrimSpace(cfg.AgentToken) != ""
	if !initialized {
		return fmt.Errorf("agent is not initialized; run `tug init` first")
	}
	fmt.Println("Restarting tug agent service...")
	msg := tryRestartAgentService()
	if strings.Contains(strings.ToLower(msg), "failed") {
		return fmt.Errorf("%s", msg)
	}
	fmt.Printf("   %s\n", msg)
	return nil
}

func stopAgentService() error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("systemctl not available")
	}
	cmd := exec.Command("systemctl", "stop", "tug-agent.service")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cannot stop tug-agent.service: %s", strings.TrimSpace(string(output)))
	}
	return nil
}

func clearAgentConnectionState(cfg config.Config) error {
	if strings.TrimSpace(cfg.AgentEnvPath) == "" {
		return fmt.Errorf("empty agent env path")
	}
	lines := []string{
		"TUG_AGENT_TOKEN=",
	}
	if cfg.APIWebSocketURL != "wss://api.tug.sh/ws/agents" {
		lines = append(lines, fmt.Sprintf("TUG_API_WS_URL=%s", cfg.APIWebSocketURL))
	}
	lines = append(lines, "")
	content := strings.Join(lines, "\n")
	return os.WriteFile(cfg.AgentEnvPath, []byte(content), 0o600)
}

func fallbackValue(value, fallback string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fallback
	}
	return trimmed
}

func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil
}

func acquireSingleInstanceLock() (func(), error) {
	lockPath := os.Getenv("TUG_AGENT_LOCK_PATH")
	if strings.TrimSpace(lockPath) == "" {
		lockPath = filepath.Join(os.TempDir(), "tug-agent.lock")
	}

	if err := os.MkdirAll(filepath.Dir(lockPath), 0o750); err != nil {
		return nil, fmt.Errorf("cannot create lock directory: %w", err)
	}

	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cannot open lock file: %w", err)
	}

	var lockErr error
	for attempts := 0; attempts < 3; attempts++ {
		lockErr = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if lockErr == nil {
			break
		}

		if attempts < 2 && (errors.Is(lockErr, syscall.EWOULDBLOCK) || errors.Is(lockErr, syscall.EAGAIN)) {
			// Read PID from lock file
			_, _ = lockFile.Seek(0, 0)
			content, readErr := io.ReadAll(lockFile)
			pidStr := strings.TrimSpace(string(content))
			pid, _ := strconv.Atoi(pidStr)

			if readErr == nil && pidStr != "" {
				if pid > 0 && !isProcessAlive(pid) {
					// Stale lock file from a dead process — truncate and try again
					_ = lockFile.Truncate(0)
					_, _ = lockFile.Seek(0, 0)
					time.Sleep(100 * time.Millisecond)
					continue
				}

				// Process is still alive or PID unknown
				shouldKill := false
				if isSystemdService() || !isTerminal(os.Stdin) {
					// Non-interactive / systemd mode: kill old process automatically on restart
					logging.Info("[lock] terminating existing agent process PID %s to start new instance", pidStr)
					shouldKill = true
				} else {
					// Interactive terminal: ask user
					fmt.Printf("⚠️ Agent is already running (PID: %s)\n", pidStr)
					fmt.Print("Do you want to kill it and start a new instance? [y/N]: ")
					reader := bufio.NewReader(os.Stdin)
					response, _ := reader.ReadString('\n')
					response = strings.TrimSpace(strings.ToLower(response))
					if response == "y" || response == "yes" {
						shouldKill = true
					}
				}

				if shouldKill && pid > 0 {
					_ = exec.Command("kill", "-9", pidStr).Run()
					time.Sleep(300 * time.Millisecond)
					continue // Try locking again
				}
			}
		}
		break
	}

	if lockErr != nil {
		_ = lockFile.Close()
		if errors.Is(lockErr, syscall.EWOULDBLOCK) || errors.Is(lockErr, syscall.EAGAIN) {
			return nil, fmt.Errorf("lock busy at %s", lockPath)
		}
		return nil, fmt.Errorf("cannot acquire lock: %w", lockErr)
	}

	if err := lockFile.Truncate(0); err == nil {
		_, _ = lockFile.Seek(0, 0)
		_, _ = lockFile.WriteString(strconv.Itoa(os.Getpid()))
	}

	release := func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	}

	return release, nil
}

func runUpdate(cfg config.Config) error {
	baseURL := cfg.APIWebSocketURL
	baseURL = strings.Replace(baseURL, "wss://", "https://", 1)
	baseURL = strings.Replace(baseURL, "ws://", "http://", 1)
	if idx := strings.Index(baseURL, "/ws/"); idx != -1 {
		baseURL = baseURL[:idx]
	}
	binaryName := fmt.Sprintf("agent-%s-%s", runtime.GOOS, runtime.GOARCH)
	binaryURL := fmt.Sprintf("%s/releases/%s?version=latest", baseURL, binaryName)

	fmt.Printf("Updating agent from: %s\n", binaryURL)
	updater := lifecycle.NewUpdater()
	return updater.SafeUpdate(context.Background(), binaryURL)
}

func printHelp(version string) {
	fmt.Printf("\033[1;36mtug\033[0m v%s - VPS control center agent\n\n", version)
	fmt.Println("\033[1;33mUSAGE:\033[0m")
	fmt.Println("  tug <command>")
	fmt.Println()
	fmt.Println("\033[1;33mCOMMANDS:\033[0m")
	fmt.Println("  \033[1;37minit\033[0m        Pair this machine (`tug init <server-id> <token>`)")
	fmt.Println("  \033[1;37mstart\033[0m       Start background agent service (`systemctl start tug-agent`)")
	fmt.Println("  \033[1;37mstatus\033[0m      Show agent connection status and service health")
	fmt.Println("  \033[1;37mstop\033[0m        Stop agent background service (`systemctl stop tug-agent`)")
	fmt.Println("  \033[1;37mrestart\033[0m     Restart agent background service (`systemctl restart tug-agent`)")
	fmt.Println("  \033[1;37mlogs\033[0m        Show last 100 agent log lines (`tug logs [n]`)")
	fmt.Println("  \033[1;37mupdate\033[0m      Update agent binary to the latest release")
	fmt.Println("  \033[1;37mdisconnect\033[0m  Disconnect agent from dashboard and reset token")
	fmt.Println("  \033[1;37mremove\033[0m      Uninstall agent and remove systemd service")
	fmt.Println("  \033[1;37mversion\033[0m     Display current agent version")
	fmt.Println("  \033[1;37mrun\033[0m         Run agent in daemon worker mode (used by systemd)")
	fmt.Println()
}

func isTerminal(file *os.File) bool {
	if file == nil {
		return false
	}
	stat, err := file.Stat()
	if err != nil {
		return false
	}
	return (stat.Mode() & os.ModeCharDevice) != 0
}

func isSystemdService() bool {
	return os.Getenv("INVOCATION_ID") != "" || os.Getenv("JOURNAL_STREAM") != ""
}
