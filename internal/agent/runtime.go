// Package agent orchestrates one running agent process: the API session, the
// command lifecycle and the local subsystems (docker, router, sandbox).
package agent

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tug.sh/services/agent/internal/config"
	"tug.sh/services/agent/internal/docker"
	"tug.sh/services/agent/internal/lifecycle"
	"tug.sh/services/agent/internal/logging"
	"tug.sh/services/agent/internal/outbox"
	"tug.sh/services/agent/internal/router"
	"tug.sh/services/agent/internal/sandbox"
)

const (
	authCooldown     = 5 * time.Minute
	pendingAuthDelay = 3 * time.Second
	minBackoffDelay  = 100 * time.Millisecond
)

type Runtime struct {
	config        config.Config
	log           *logging.Logger
	fileManager   *sandbox.FileManager
	dockerManager *docker.Manager
	router        *router.Router
	updater       *lifecycle.Updater
	writeMu       sync.Mutex
	eventQueue    *outbox.Queue
	commandInbox  *commandInbox

	termMu                  sync.Mutex
	terminals               map[string]*TerminalSession
	containerDeltaStateMu   sync.Mutex
	lastContainerDeltaState map[string]string
	ackStateMu              sync.Mutex
	lastAckSeq              uint64
	lastAckProgressAt       time.Time
	lastQueueResetAt        time.Time
}

func NewRuntime(cfg config.Config) (*Runtime, error) {
	logger := LoggerForConfig(cfg)
	queue := outbox.NewQueue(cfg.OutboxPath)
	// Loading is unconditional now. The queue used to be behind a feature flag,
	// so an agent started with it off dropped everything the previous run had
	// not managed to deliver.
	if err := queue.Load(); err != nil {
		logger.Warn("cannot load the outbox: %v", err)
	}
	dockerManager := docker.NewManager()
	return &Runtime{
		config:                  cfg,
		log:                     logger,
		fileManager:             sandbox.NewFileManager(),
		dockerManager:           dockerManager,
		router:                  router.New(dockerManager, router.SpecFromConfig(cfg)),
		updater:                 lifecycle.NewUpdater(),
		eventQueue:              queue,
		commandInbox:            newCommandInbox(commandInboxPath(cfg)),
		terminals:               make(map[string]*TerminalSession),
		lastContainerDeltaState: map[string]string{},
		lastAckProgressAt:       time.Now(),
	}, nil
}

func commandInboxPath(cfg config.Config) string {
	if path := strings.TrimSpace(cfg.CommandInboxPath); path != "" {
		return path
	}
	return filepath.Join(sandbox.DataDir(), "agent-command-inbox.json")
}

// LoggerForConfig builds the agent logger from configuration: an explicit
// TUG_LOG_LEVEL wins, otherwise the legacy verbose switch selects debug.
func LoggerForConfig(cfg config.Config) *logging.Logger {
	fallback := logging.LevelInfo
	if cfg.Verbose {
		fallback = logging.LevelDebug
	}
	return logging.New(logging.ParseLevel(cfg.LogLevel, fallback))
}

// Run keeps a session to the API alive for the lifetime of ctx, reconnecting
// with jittered backoff and pausing on unrecoverable auth errors.
func (runtime *Runtime) Run(ctx context.Context) error {
	if strings.TrimSpace(runtime.config.AgentToken) == "" || strings.TrimSpace(runtime.config.ServerID) == "" {
		runtime.logReconnectRecoveryHint(fmt.Errorf("agent is not initialized (missing or invalid token)"))
		<-ctx.Done()
		return nil
	}
	consecutiveFailures := 0

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		runtime.log.Info("connection attempt (failure_streak=%d)", consecutiveFailures)
		connected, err := runtime.connectAndServe(ctx)
		if err != nil {
			runtime.log.Warn("connection closed: %v", err)
			if isNonRetriable(err) {
				runtime.logReconnectRecoveryHint(err)
				runtime.log.Warn("auth/config error; cooling down for %s before retrying...", authCooldown)
				if !sleepUntil(ctx, authCooldown) {
					return nil
				}
				consecutiveFailures = 0
				continue
			}
		}
		if connected {
			// Reset retry backoff when the socket session was stable.
			consecutiveFailures = 0
			continue
		}
		consecutiveFailures++
		waitDelay := jitteredBackoff(
			runtime.config.ReconnectBaseDelay,
			runtime.config.ReconnectMaxDelay,
			consecutiveFailures-1,
			runtime.config.ReconnectJitterPct,
		)
		if isPendingAuthError(err) {
			// Pairing is pending in the dashboard, not broken: retry promptly.
			waitDelay = pendingAuthDelay
			consecutiveFailures = 0
		}
		runtime.log.Info("reconnect scheduled in %s (failure_streak=%d)", waitDelay, consecutiveFailures)
		if !sleepUntil(ctx, waitDelay) {
			return nil
		}
	}
}

func (runtime *Runtime) logReconnectRecoveryHint(reason error) {
	envPath := strings.TrimSpace(runtime.config.AgentEnvPath)
	if envPath == "" {
		envPath = "/etc/tug/agent.env"
	}
	runtime.log.Warn("reconnect disabled: %v", reason)
	runtime.log.Warn("recovery steps:")
	runtime.log.Warn("1) Verify agent state: sudo tug status")
	runtime.log.Warn("2) Re-initialize token and link: sudo tug init")
	runtime.log.Warn("3) Open generated /connect/<token> URL in dashboard to authorize this host")
	runtime.log.Warn("4) Ensure %s contains valid TUG_AGENT_TOKEN", envPath)
}

// sleepUntil waits for the delay and reports false when ctx ended first.
func sleepUntil(ctx context.Context, delay time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(delay):
		return true
	}
}

func positiveOrDefault(value time.Duration, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return value
}

// jitteredBackoff doubles the delay per step up to max and spreads it by
// jitterPct, so a fleet of agents does not reconnect in lockstep.
func jitteredBackoff(base, max time.Duration, step int, jitterPct int) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	if max < base {
		max = base
	}
	if step < 0 {
		step = 0
	}
	delay := base
	for i := 0; i < step && delay < max; i++ {
		delay *= 2
		if delay > max {
			delay = max
		}
	}
	if jitterPct <= 0 {
		return delay
	}
	jitterMax := delay * time.Duration(jitterPct) / 100
	if jitterMax <= 0 {
		return delay
	}
	// Randomize in range [-jitterMax, +jitterMax].
	offset, err := rand.Int(rand.Reader, big.NewInt(int64(jitterMax*2+1)))
	if err != nil {
		return delay
	}
	adjusted := delay + time.Duration(offset.Int64()) - jitterMax
	switch {
	case adjusted < minBackoffDelay:
		return minBackoffDelay
	case adjusted > max:
		return max
	default:
		return adjusted
	}
}
