package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"time"
)

type Updater struct{}

func NewUpdater() *Updater {
	return &Updater{}
}

func (u *Updater) SafeUpdate(ctx context.Context, binaryURL string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, binaryURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create update request: %w", err)
	}
	response, err := (&http.Client{Timeout: 60 * time.Second}).Do(request)
	if err != nil {
		return fmt.Errorf("failed to download update binary: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download server returned HTTP status %d", response.StatusCode)
	}

	currentBinary, err := os.Executable()
	if err != nil {
		currentBinary = "/usr/local/bin/tug-agent"
	}
	nextBinary := currentBinary + ".next"

	payload, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("failed to read binary payload: %w", err)
	}
	if len(payload) < 1000 {
		return fmt.Errorf("invalid binary size (%d bytes)", len(payload))
	}

	if err := os.WriteFile(nextBinary, payload, 0o755); err != nil {
		return fmt.Errorf("failed to write temporary binary: %w", err)
	}

	testCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	testCmd := exec.CommandContext(testCtx, nextBinary, "--test-mode")
	if err := testCmd.Start(); err != nil {
		_ = os.Remove(nextBinary)
		return fmt.Errorf("new binary execution test failed: %w", err)
	}
	if err := testCmd.Wait(); err != nil {
		_ = os.Remove(nextBinary)
		return fmt.Errorf("new binary failed health test: %w", err)
	}

	if err := os.Rename(nextBinary, currentBinary); err != nil {
		return fmt.Errorf("failed to replace agent binary: %w", err)
	}

	restart := exec.Command("systemctl", "restart", "tug-agent.service")
	if _, err := restart.CombinedOutput(); err != nil {
		// Fallback: process exit so supervisor/systemd/docker restarts it with new binary
		go func() {
			time.Sleep(1 * time.Second)
			os.Exit(0)
		}()
	}
	return nil
}
