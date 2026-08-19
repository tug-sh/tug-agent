// Package shell runs host processes on behalf of the agent and records their
// output in an operation transcript, so every component reports failures to the
// dashboard the same way.
package shell

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"tug.sh/services/agent/internal/logging"
)

// Custom builds a project supplied command line, which always runs through sh
// because projects express it as a single shell string.
func Custom(ctx context.Context, commandLine string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", commandLine)
}

// RunTracked executes a process, records its output in the transcript and wraps
// a failure with the caller's message, preferring the process output as the
// reason because it is what the user needs to see in the dashboard.
func RunTracked(
	ctx context.Context,
	transcript *logging.Transcript,
	failureMessage string,
	name string,
	args ...string,
) error {
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	outputText := strings.TrimSpace(string(output))
	transcript.AddCommandOutput(outputText)
	if err == nil {
		return nil
	}
	if outputText != "" {
		return fmt.Errorf("%s: %s", failureMessage, outputText)
	}
	return fmt.Errorf("%s: %w", failureMessage, err)
}
