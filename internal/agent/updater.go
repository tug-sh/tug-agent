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
	downloadCtx, cancelDownload := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelDownload()

	request, err := http.NewRequestWithContext(downloadCtx, http.MethodGet, binaryURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create update request: %w", err)
	}
	client := &http.Client{Timeout: 90 * time.Second}
	response, err := client.Do(request)
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

	testCtx, cancelTest := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelTest()

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

	return nil
}
