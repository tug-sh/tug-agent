// Package logging provides the single logging entry point shared by every
// agent component: the CLI, the runtime and the background workers.
package logging

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
)

type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

func (level Level) tag() string {
	switch level {
	case LevelDebug:
		return "[debug]"
	case LevelWarn:
		return "[warn]"
	case LevelError:
		return "[error]"
	default:
		return "[info]"
	}
}

// ParseLevel maps a human readable level name to a Level, falling back to the
// provided default for unknown values.
func ParseLevel(value string, fallback Level) Level {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug", "verbose", "trace":
		return LevelDebug
	case "info":
		return LevelInfo
	case "warn", "warning":
		return LevelWarn
	case "error", "fatal":
		return LevelError
	default:
		return fallback
	}
}

// Logger writes leveled messages through the standard library logger, so the
// destination configured by the agent process (stdout plus rotating file) is
// respected without any extra wiring.
type Logger struct {
	mu       sync.RWMutex
	minLevel Level
	sink     *log.Logger
}

func New(minLevel Level) *Logger {
	return &Logger{minLevel: minLevel}
}

// WithSink returns a logger writing to an explicit destination instead of the
// standard library default. Used by tests and by the CLI subcommands.
func (logger *Logger) WithSink(sink *log.Logger) *Logger {
	return &Logger{minLevel: logger.MinLevel(), sink: sink}
}

func (logger *Logger) SetMinLevel(level Level) {
	logger.mu.Lock()
	defer logger.mu.Unlock()
	logger.minLevel = level
}

func (logger *Logger) MinLevel() Level {
	logger.mu.RLock()
	defer logger.mu.RUnlock()
	return logger.minLevel
}

func (logger *Logger) Enabled(level Level) bool {
	return level >= logger.MinLevel()
}

func (logger *Logger) Debug(format string, args ...any) {
	logger.write(LevelDebug, format, args...)
}

func (logger *Logger) Info(format string, args ...any) {
	logger.write(LevelInfo, format, args...)
}

func (logger *Logger) Warn(format string, args ...any) {
	logger.write(LevelWarn, format, args...)
}

func (logger *Logger) Error(format string, args ...any) {
	logger.write(LevelError, format, args...)
}

// Fatal reports an unrecoverable condition and terminates the process.
func (logger *Logger) Fatal(format string, args ...any) {
	logger.write(LevelError, format, args...)
	os.Exit(1)
}

func (logger *Logger) write(level Level, format string, args ...any) {
	if !logger.Enabled(level) {
		return
	}
	line := level.tag() + " " + fmt.Sprintf(format, args...)

	logger.mu.RLock()
	sink := logger.sink
	logger.mu.RUnlock()

	const callDepth = 3
	if sink != nil {
		_ = sink.Output(callDepth, line)
		return
	}
	_ = log.Output(callDepth, line)
}

var (
	defaultMu     sync.RWMutex
	defaultLogger = New(LevelInfo)
)

// Default returns the process wide logger used by components that are not
// bound to a Runtime instance.
func Default() *Logger {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultLogger
}

func SetDefault(logger *Logger) {
	if logger == nil {
		return
	}
	defaultMu.Lock()
	defer defaultMu.Unlock()
	defaultLogger = logger
}

func Debug(format string, args ...any) { Default().Debug(format, args...) }
func Info(format string, args ...any)  { Default().Info(format, args...) }
func Warn(format string, args ...any)  { Default().Warn(format, args...) }
func Error(format string, args ...any) { Default().Error(format, args...) }
func Fatal(format string, args ...any) { Default().Fatal(format, args...) }
