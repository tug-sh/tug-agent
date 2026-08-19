package lifecycle

import (
	"bytes"
	"io"
	"testing"
	"time"
)

func TestProgressReaderReportsPercent(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 100)
	var reported []int

	reader := &progressReader{
		reader: bytes.NewReader(payload),
		total:  uint64(len(payload)),
		onProgress: func(downloaded uint64, total uint64, percent int) {
			if total != uint64(len(payload)) {
				t.Errorf("total = %d, want %d", total, len(payload))
			}
			reported = append(reported, percent)
		},
	}

	buffer := make([]byte, 25)
	if _, err := io.CopyBuffer(io.Discard, reader, buffer); err != nil {
		t.Fatalf("copy failed: %v", err)
	}
	if len(reported) == 0 {
		t.Fatal("expected progress reports")
	}
	if reported[len(reported)-1] != 100 {
		t.Fatalf("last report = %d%%, want 100%%", reported[len(reported)-1])
	}
}

// Progress frames travel over the websocket, so repeated percentages within
// the report interval must be dropped.
func TestProgressReaderThrottlesRepeatedPercent(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 1000)
	reports := 0

	reader := &progressReader{
		reader:     bytes.NewReader(payload),
		total:      uint64(len(payload)),
		lastPct:    0,
		lastReport: time.Now(),
		onProgress: func(uint64, uint64, int) { reports++ },
	}

	buffer := make([]byte, 1)
	for index := 0; index < 5; index++ {
		if _, err := reader.Read(buffer); err != nil {
			t.Fatalf("read failed: %v", err)
		}
	}
	if reports != 0 {
		t.Fatalf("expected the identical percentage to be throttled, got %d reports", reports)
	}
}

func TestProgressReaderWithoutTotal(t *testing.T) {
	reports := 0
	reader := &progressReader{
		reader:     bytes.NewReader([]byte("payload")),
		onProgress: func(uint64, uint64, int) { reports++ },
	}

	if _, err := io.ReadAll(reader); err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if reports != 0 {
		t.Fatal("an unknown content length cannot produce a percentage")
	}
	if reader.downloaded != uint64(len("payload")) {
		t.Fatalf("downloaded = %d, want %d", reader.downloaded, len("payload"))
	}
}
