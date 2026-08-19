package logging

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]Level{
		"debug":   LevelDebug,
		"VERBOSE": LevelDebug,
		" trace ": LevelDebug,
		"info":    LevelInfo,
		"warning": LevelWarn,
		"error":   LevelError,
		"fatal":   LevelError,
	}
	for input, want := range cases {
		if got := ParseLevel(input, LevelInfo); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", input, got, want)
		}
	}
	if got := ParseLevel("nonsense", LevelWarn); got != LevelWarn {
		t.Errorf("unknown level should fall back, got %v", got)
	}
}

func TestLoggerFiltersBelowMinLevel(t *testing.T) {
	var buffer bytes.Buffer
	logger := New(LevelWarn).WithSink(log.New(&buffer, "", 0))

	logger.Debug("hidden debug")
	logger.Info("hidden info")
	logger.Warn("visible warn")
	logger.Error("visible error %d", 7)

	output := buffer.String()
	if strings.Contains(output, "hidden") {
		t.Fatalf("messages below the minimum level leaked: %q", output)
	}
	if !strings.Contains(output, "[warn] visible warn") {
		t.Fatalf("missing warn line in %q", output)
	}
	if !strings.Contains(output, "[error] visible error 7") {
		t.Fatalf("missing formatted error line in %q", output)
	}
}

func TestLoggerSetMinLevel(t *testing.T) {
	var buffer bytes.Buffer
	logger := New(LevelError).WithSink(log.New(&buffer, "", 0))

	logger.Debug("first")
	if buffer.Len() != 0 {
		t.Fatalf("expected no output at error level, got %q", buffer.String())
	}

	logger.SetMinLevel(LevelDebug)
	if !logger.Enabled(LevelDebug) {
		t.Fatal("debug should be enabled after lowering the level")
	}
	logger.Debug("second")
	if !strings.Contains(buffer.String(), "[debug] second") {
		t.Fatalf("expected debug output, got %q", buffer.String())
	}
}

func TestSetDefaultIgnoresNil(t *testing.T) {
	original := Default()
	defer SetDefault(original)

	replacement := New(LevelError)
	SetDefault(replacement)
	SetDefault(nil)

	if Default() != replacement {
		t.Fatal("nil must not replace the default logger")
	}
}
