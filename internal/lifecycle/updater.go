// Package lifecycle manages the agent's own installation on the host: self
// update of the binary and detached uninstall.
package lifecycle

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

type Updater struct{}

func NewUpdater() *Updater {
	return &Updater{}
}

type ProgressFunc func(downloaded uint64, total uint64, percent int)

// progressReportInterval throttles download progress frames sent to the API.
const progressReportInterval = 250 * time.Millisecond

type progressReader struct {
	reader     io.Reader
	total      uint64
	downloaded uint64
	lastReport time.Time
	lastPct    int
	onProgress ProgressFunc
}

func (progress *progressReader) Read(buffer []byte) (int, error) {
	bytesRead, err := progress.reader.Read(buffer)
	if bytesRead == 0 {
		return bytesRead, err
	}
	progress.downloaded += uint64(bytesRead)
	if progress.total == 0 || progress.onProgress == nil {
		return bytesRead, err
	}

	percent := int((progress.downloaded * 100) / progress.total)
	now := time.Now()
	if percent == progress.lastPct && now.Sub(progress.lastReport) <= progressReportInterval {
		return bytesRead, err
	}
	progress.lastPct = percent
	progress.lastReport = now
	progress.onProgress(progress.downloaded, progress.total, percent)
	return bytesRead, err
}

func (updater *Updater) SafeUpdate(ctx context.Context, binaryURL string) error {
	return updater.SafeUpdateWithProgress(ctx, binaryURL, nil)
}

func (updater *Updater) SafeUpdateWithProgress(ctx context.Context, binaryURL string, onProgress ProgressFunc) error {
	downloadCtx, cancelDownload := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancelDownload()

	request, err := http.NewRequestWithContext(downloadCtx, http.MethodGet, binaryURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create update request: %w", err)
	}
	client := &http.Client{Timeout: 120 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("failed to download update binary: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download server returned HTTP status %d", response.StatusCode)
	}

	totalBytes := uint64(0)
	if response.ContentLength > 0 {
		totalBytes = uint64(response.ContentLength)
	}

	currentBinary, err := os.Executable()
	if err == nil {
		currentBinary = strings.TrimSpace(strings.TrimSuffix(currentBinary, " (deleted)"))
	} else {
		currentBinary = "/usr/local/bin/tug-agent"
	}
	nextBinary := currentBinary + ".next"

	var reader io.Reader = response.Body
	if onProgress != nil {
		reader = &progressReader{
			reader:     response.Body,
			total:      totalBytes,
			onProgress: onProgress,
		}
	}

	payload, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("failed to read binary payload: %w", err)
	}
	if len(payload) < 1000 {
		return fmt.Errorf("invalid binary size (%d bytes)", len(payload))
	}

	if onProgress != nil && totalBytes > 0 {
		onProgress(uint64(len(payload)), uint64(len(payload)), 100)
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
