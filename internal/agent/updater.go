package agent

import (
	"context"
	"fmt"
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
		return err
	}
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	nextBinary := "/usr/local/bin/tug.next"
	currentBinary := "/usr/local/bin/tug"

	payload, err := ioReadAll(response.Body)
	if err != nil {
		return err
	}
	if err := os.WriteFile(nextBinary, payload, 0o755); err != nil {
		return err
	}

	testCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	testCmd := exec.CommandContext(testCtx, nextBinary, "--test-mode")
	if err := testCmd.Start(); err != nil {
		_ = os.Remove(nextBinary)
		return err
	}
	if err := testCmd.Wait(); err != nil {
		_ = os.Remove(nextBinary)
		return fmt.Errorf("new binary failed health test: %w", err)
	}

	if err := os.Rename(nextBinary, currentBinary); err != nil {
		return err
	}

	restart := exec.Command("systemctl", "restart", "tug-agent.service")
	if output, err := restart.CombinedOutput(); err != nil {
		return fmt.Errorf("cannot restart systemd service: %s: %w", string(output), err)
	}
	return nil
}
