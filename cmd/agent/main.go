package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"

	"tug.sh/services/agent/internal/agent"
	"tug.sh/services/agent/internal/config"
)

func main() {
	initMode := flag.Bool("init", false, "Generate connection key and dashboard URL (`tug init`)")
	startMode := flag.Bool("start", false, "Start agent service in background (initiates connection setup if not configured, `tug start`)")
	statusMode := flag.Bool("status", false, "Show agent status (`tug status`)")
	stopMode := flag.Bool("stop", false, "Stop agent service (`tug stop`)")
	disconnectMode := flag.Bool("disconnect", false, "Disconnect agent from dashboard (`tug disconnect`)")
	removeMode := flag.Bool("remove", false, "Uninstall agent and remove service (`tug remove`)")
	updateMode := flag.Bool("update", false, "Update agent binary to the latest version (`tug update`)")
	versionMode := flag.Bool("version", false, "Show agent version (`tug version`)")
	runMode := flag.Bool("run", false, "Run agent in daemon mode (`tug run`)")
	helpMode := flag.Bool("help", false, "Show help and available commands")
	testMode := flag.Bool("test-mode", false, "Run in test mode for updater health check")
	verbose := flag.Bool("verbose", true, "Enable verbose operation logs")

	flag.Usage = func() {
		cfg := config.Load()
		printHelp(cfg.AgentVersion)
	}

	flag.Parse()

	if *testMode {
		fmt.Println("health-check: ok")
		return
	}

	if err := loadAgentEnvFile(); err != nil {
		log.Fatalf("failed to load environment file: %v", err)
	}

	cfg := config.Load()

	if *helpMode || hasCommand(flag.Args(), "help") || hasCommand(flag.Args(), "-h") || hasCommand(flag.Args(), "--help") {
		printHelp(cfg.AgentVersion)
		return
	}

	if *versionMode || hasCommand(flag.Args(), "version") {
		fmt.Printf("tug-agent v%s\n", cfg.AgentVersion)
		return
	}

	if *updateMode || hasCommand(flag.Args(), "update") {
		if err := runUpdate(cfg); err != nil {
			log.Fatalf("update failed: %v", err)
		}
		fmt.Println("Agent updated successfully and restarted.")
		return
	}

	if *initMode || hasCommand(flag.Args(), "init") {
		if err := runInit(cfg); err != nil {
			log.Fatalf("init failed: %v", err)
		}
		return
	}
	if *startMode || hasCommand(flag.Args(), "start") {
		if err := runStart(cfg); err != nil {
			log.Fatalf("start failed: %v", err)
		}
		return
	}
	if *statusMode || hasCommand(flag.Args(), "status") {
		if err := runStatus(); err != nil {
			log.Fatalf("status failed: %v", err)
		}
		return
	}
	if *stopMode || hasCommand(flag.Args(), "stop") {
		if err := stopAgentService(); err != nil {
			log.Fatalf("stop failed: %v", err)
		}
		fmt.Println("Agent stopped.")
		return
	}
	if *disconnectMode || hasCommand(flag.Args(), "disconnect") {
		if err := stopAgentService(); err != nil {
			log.Printf("warning: cannot stop service automatically: %v", err)
		}
		if err := clearAgentConnectionState(cfg); err != nil {
			log.Fatalf("disconnect failed: %v", err)
		}
		fmt.Println("Agent disconnected from dashboard. Run `tug --init` to reconnect.")
		return
	}
	if *removeMode || hasCommand(flag.Args(), "remove") {
		if err := agent.RunDetachedUninstall(false); err != nil {
			log.Fatalf("remove failed: %v", err)
		}
		fmt.Println("Agent uninstall started in background.")
		return
	}

	isRunDaemon := *runMode || hasCommand(flag.Args(), "run") || hasCommand(flag.Args(), "daemon") || hasCommand(flag.Args(), "run-service") || hasCommand(flag.Args(), "service")

	if isSystemdService() || (!isTerminal(os.Stdout) && !isTerminal(os.Stdin) && len(flag.Args()) == 0 && flag.NFlag() == 0) {
		isRunDaemon = true
	}

	if !isRunDaemon {
		printHelp(cfg.AgentVersion)
		return
	}

	cfg = config.Load()
	cfg.Verbose = *verbose

	logPath := filepath.Join(agent.GetDataDir(), "logs", "agent.log")
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
		log.Fatalf("agent is already running: %v", err)
	}
	defer releaseLock()
	if cfg.Verbose {
		log.Printf(
			"agent verbose enabled: server_id=%s workspace_id=%s ws_url=%s env_path=%s profile=%s heartbeat=%s self_heal=%s reconnect_base=%s reconnect_max=%s jitter_pct=%d",
			cfg.ServerID,
			cfg.WorkspaceID,
			cfg.APIWebSocketURL,
			cfg.AgentEnvPath,
			cfg.TrafficProfile,
			cfg.HeartbeatInterval,
			cfg.SelfHealInterval,
			cfg.ReconnectBaseDelay,
			cfg.ReconnectMaxDelay,
			cfg.ReconnectJitterPct,
		)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	runtimeAgent, err := agent.NewRuntime(cfg)
	if err != nil {
		log.Fatalf("failed to create runtime: %v", err)
	}

	if err := runtimeAgent.Run(ctx); err != nil {
		log.Fatalf("agent run failed: %v", err)
	}
}

func hasCommand(args []string, command string) bool {
	if len(args) == 0 {
		return false
	}
	return strings.TrimSpace(args[0]) == command
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
	fmt.Printf("server_id: %s\n", fallbackValue(cfg.ServerID, "(not set)"))
	fmt.Printf("agent_token: %s\n", tokenPreview)
	fmt.Printf("ws_url: %s\n", fallbackValue(cfg.APIWebSocketURL, "(not set)"))
	fmt.Printf("env_path: %s\n", fallbackValue(cfg.AgentEnvPath, "(not set)"))
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
	for attempts := 0; attempts < 2; attempts++ {
		lockErr = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if lockErr == nil {
			break
		}

		if attempts == 0 && (errors.Is(lockErr, syscall.EWOULDBLOCK) || errors.Is(lockErr, syscall.EAGAIN)) {
			// Read PID from file
			_, _ = lockFile.Seek(0, 0)
			content, readErr := io.ReadAll(lockFile)
			pidStr := strings.TrimSpace(string(content))

			if readErr == nil && pidStr != "" {
				fmt.Printf("⚠️ Agent is already running (PID: %s)\n", pidStr)
				fmt.Print("Do you want to kill it and start a new instance? [y/N]: ")
				reader := bufio.NewReader(os.Stdin)
				response, _ := reader.ReadString('\n')
				response = strings.TrimSpace(strings.ToLower(response))

				if response == "y" || response == "yes" {
					killCmd := exec.Command("kill", "-9", pidStr)
					if err := killCmd.Run(); err == nil {
						fmt.Printf("Process %s killed.\n", pidStr)
						time.Sleep(500 * time.Millisecond)
						continue // Try locking again
					} else {
						fmt.Printf("Failed to kill process %s: %v\n", pidStr, err)
					}
				}
			}
		}
		break // Exit loop if we're not retrying
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
	updater := agent.NewUpdater()
	return updater.SafeUpdate(context.Background(), binaryURL)
}

func printHelp(version string) {
	fmt.Printf("\033[1;36mtug\033[0m v%s - VPS control center agent\n\n", version)
	fmt.Println("\033[1;33mUSAGE:\033[0m")
	fmt.Println("  tug <command> [flags]")
	fmt.Println()
	fmt.Println("\033[1;33mCOMMANDS:\033[0m")
	fmt.Println("  \033[1;37minit\033[0m        Generate connection pairing key and dashboard link")
	fmt.Println("  \033[1;37mstart\033[0m       Start background agent service (`systemctl start tug-agent`)")
	fmt.Println("  \033[1;37mstatus\033[0m      Show agent connection status and service health")
	fmt.Println("  \033[1;37mstop\033[0m        Stop agent background service (`systemctl stop tug-agent`)")
	fmt.Println("  \033[1;37mupdate\033[0m      Update agent binary to the latest release")
	fmt.Println("  \033[1;37mdisconnect\033[0m  Disconnect agent from dashboard and reset token")
	fmt.Println("  \033[1;37mremove\033[0m      Uninstall agent and remove systemd service")
	fmt.Println("  \033[1;37mversion\033[0m     Display current agent version")
	fmt.Println("  \033[1;37mrun\033[0m         Run agent in daemon worker mode (used by systemd)")
	fmt.Println()
	fmt.Println("\033[1;33mFLAGS:\033[0m")
	fmt.Println("  --init, --start, --status, --stop, --update, --disconnect, --remove, --version, --help")
	fmt.Println()
}
