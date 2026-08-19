package agent

import (
	"context"
	"testing"
	"time"

	"tug.sh/services/agent/internal/config"
	"tug.sh/services/agent/internal/logging"
)

func TestJitteredBackoffGrowsAndCaps(t *testing.T) {
	base := time.Second
	max := 8 * time.Second

	cases := map[int]time.Duration{0: base, 1: 2 * time.Second, 2: 4 * time.Second, 3: max, 10: max}
	for step, want := range cases {
		if got := jitteredBackoff(base, max, step, 0); got != want {
			t.Errorf("jitteredBackoff(step=%d) = %s, want %s", step, got, want)
		}
	}
	if got := jitteredBackoff(base, max, -5, 0); got != base {
		t.Errorf("a negative step must behave like the first attempt, got %s", got)
	}
}

func TestJitteredBackoffStaysWithinBounds(t *testing.T) {
	base := 2 * time.Second
	max := 10 * time.Second

	for attempt := 0; attempt < 200; attempt++ {
		delay := jitteredBackoff(base, max, 3, 50)
		if delay < minBackoffDelay || delay > max {
			t.Fatalf("delay %s outside [%s, %s]", delay, minBackoffDelay, max)
		}
	}
}

func TestJitteredBackoffRepairsInvalidBounds(t *testing.T) {
	if got := jitteredBackoff(0, 0, 0, 0); got != time.Second {
		t.Errorf("a zero base must fall back to 1s, got %s", got)
	}
	if got := jitteredBackoff(5*time.Second, time.Second, 3, 0); got != 5*time.Second {
		t.Errorf("a max below the base must be lifted to the base, got %s", got)
	}
}

func TestPositiveOrDefault(t *testing.T) {
	if got := positiveOrDefault(0, time.Minute); got != time.Minute {
		t.Errorf("got %s, want the fallback", got)
	}
	if got := positiveOrDefault(-time.Second, time.Minute); got != time.Minute {
		t.Errorf("got %s, want the fallback", got)
	}
	if got := positiveOrDefault(time.Second, time.Minute); got != time.Second {
		t.Errorf("got %s, want the configured value", got)
	}
}

func TestSleepUntilReportsCancellation(t *testing.T) {
	if !sleepUntil(context.Background(), time.Millisecond) {
		t.Error("expected a completed sleep to report true")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepUntil(ctx, time.Minute) {
		t.Error("expected a cancelled context to report false")
	}
}

func TestLoggerForConfig(t *testing.T) {
	if got := LoggerForConfig(config.Config{}).MinLevel(); got != logging.LevelInfo {
		t.Errorf("MinLevel = %v, want info", got)
	}
	if got := LoggerForConfig(config.Config{Verbose: true}).MinLevel(); got != logging.LevelDebug {
		t.Errorf("verbose must select debug, got %v", got)
	}
	// An explicit level wins over the legacy verbose switch.
	if got := LoggerForConfig(config.Config{Verbose: true, LogLevel: "warn"}).MinLevel(); got != logging.LevelWarn {
		t.Errorf("MinLevel = %v, want warn", got)
	}
}

// An agent without a token must idle instead of hammering the API, and must
// still stop as soon as its context ends.
func TestRunWithoutTokenWaitsForContext(t *testing.T) {
	runtime, err := NewRuntime(config.Config{})
	if err != nil {
		t.Fatalf("NewRuntime failed: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()

	select {
	case runErr := <-done:
		if runErr != nil {
			t.Fatalf("Run returned %v, want nil", runErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after its context was cancelled")
	}
}
