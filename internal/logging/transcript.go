package logging

import (
	"fmt"
	"strings"
)

// Transcript collects the human readable log of a single operation
// (deployment, router change, prune, ...). It is returned to the dashboard as
// command result logs, which is a different channel than the leveled agent
// logger, and can optionally mirror every line into that logger.
type Transcript struct {
	lines  []string
	mirror *Logger
	prefix string
}

func NewTranscript(initialLines ...string) *Transcript {
	transcript := &Transcript{lines: make([]string, 0, len(initialLines)+4)}
	transcript.lines = append(transcript.lines, initialLines...)
	return transcript
}

// MirrorTo also writes every transcript line to the agent log as debug output,
// so a long running operation can be followed live in `tug logs`.
func (transcript *Transcript) MirrorTo(logger *Logger, prefix string) *Transcript {
	transcript.mirror = logger
	transcript.prefix = prefix
	return transcript
}

func (transcript *Transcript) add(line string) {
	transcript.lines = append(transcript.lines, line)
	if transcript.mirror != nil {
		transcript.mirror.Debug("%s%s", transcript.prefix, line)
	}
}

// Addf appends a single formatted line.
func (transcript *Transcript) Addf(format string, args ...any) {
	transcript.add(fmt.Sprintf(format, args...))
}

// AddCommandOutput appends raw process output, one entry per non-empty line.
func (transcript *Transcript) AddCommandOutput(raw string) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return
	}
	for _, line := range strings.Split(trimmed, "\n") {
		transcript.add(line)
	}
}

// Merge appends the transcript of a nested operation.
func (transcript *Transcript) Merge(lines []string) {
	for _, line := range lines {
		transcript.add(line)
	}
}

// Lines returns the collected transcript.
func (transcript *Transcript) Lines() []string {
	return transcript.lines
}

// Fail returns the transcript together with the error, matching the
// (logs, error) contract every command handler uses.
func (transcript *Transcript) Fail(format string, args ...any) ([]string, error) {
	return transcript.lines, fmt.Errorf(format, args...)
}

// Done appends a closing line and returns the successful transcript.
func (transcript *Transcript) Done(message string) ([]string, error) {
	transcript.add(message)
	return transcript.lines, nil
}
