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

type ProgressFunc func(downloaded uint64, total uint64, percent int)

type progressReader struct {
	reader     io.Reader
	total      uint64
	downloaded uint64
	lastReport time.Time
	lastPct    int
	onProgress ProgressFunc
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.reader.Read(p)
	if n > 0 {
		pr.downloaded += uint64(n)
		if pr.total > 0 && pr.onProgress != nil {
			pct := int((pr.downloaded * 100) / pr.total)
			now := time.Now()
			if pct != pr.lastPct || now.Sub(pr.lastReport) > 250*time.Millisecond {
				pr.lastPct = pct
				pr.lastReport = now
				pr.onProgress(pr.downloaded, pr.total, pct)
			}
		}
	}
	return n, err
}

func (u *Updater) SafeUpdate(ctx context.Context, binaryURL string) error {
	return u.SafeUpdateWithProgress(ctx, binaryURL, nil)
}

func (u *Updater) SafeUpdateWithProgress(ctx context.Context, binaryURL string, onProgress ProgressFunc) error {
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
	if err != nil {
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
