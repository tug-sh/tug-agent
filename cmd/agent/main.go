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
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"

	"tug.sh/services/agent/internal/agent"
	"tug.sh/services/agent/internal/config"
)

func main() {
	initMode := flag.Bool("init", false, "Generate connection key and dashboard URL")
	statusMode := flag.Bool("status", false, "Show agent status")
	stopMode := flag.Bool("stop", false, "Stop agent service")
	disconnectMode := flag.Bool("disconnect", false, "Disconnect agent from dashboard")
	removeMode := flag.Bool("remove", false, "Uninstall agent and remove service")
	verbose := flag.Bool("verbose", true, "Enable verbose operation logs")
	flag.Parse()

	if err := loadAgentEnvFile(); err != nil {
		log.Fatalf("failed to load environment file: %v", err)
	}

	cfg := config.Load()
	if *initMode || hasCommand(flag.Args(), "init") {
		if err := runInit(cfg); err != nil {
			log.Fatalf("init failed: %v", err)
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
			"agent verbose enabled: server_id=%s workspace_id=%s ws_url=%s env_path=%s",
			cfg.ServerID,
			cfg.WorkspaceID,
			cfg.APIWebSocketURL,
			cfg.AgentEnvPath,
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
	apiWS := strings.TrimSpace(cfg.APIWebSocketURL)
	if apiWS == "" {
		apiWS = "wss://api.tug.sh/ws/agents"
	}
	content := strings.Join([]string{
		"TUG_AGENT_TOKEN=",
		fmt.Sprintf("TUG_API_WS_URL=%s", apiWS),
		"",
	}, "\n")
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
