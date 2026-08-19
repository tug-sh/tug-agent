package shell

import (
	"context"
	"strings"
	"testing"

	"tug.sh/services/agent/internal/logging"
)

func TestCustomRunsThroughShell(t *testing.T) {
	cmd := Custom(context.Background(), "echo hi")
	if !strings.HasSuffix(cmd.Path, "sh") {
		t.Errorf("Path = %q, want a shell", cmd.Path)
	}
	if len(cmd.Args) != 3 || cmd.Args[1] != "-c" || cmd.Args[2] != "echo hi" {
		t.Errorf("unexpected args %v", cmd.Args)
	}
}

func TestRunTrackedRecordsOutput(t *testing.T) {
	transcript := logging.NewTranscript()
	if err := RunTracked(context.Background(), transcript, "echo failed", "sh", "-c", "echo first; echo second"); err != nil {
		t.Fatalf("RunTracked failed: %v", err)
	}
	lines := transcript.Lines()
	if len(lines) != 2 || lines[0] != "first" || lines[1] != "second" {
		t.Fatalf("transcript = %v, want both output lines", lines)
	}
}

// The dashboard shows the transcript, so a failure must carry the process
// output rather than a bare exit status.
func TestRunTrackedPrefersProcessOutput(t *testing.T) {
	transcript := logging.NewTranscript()
	err := RunTracked(context.Background(), transcript, "clone failed", "sh", "-c", "echo permission denied >&2; exit 1")
	if err == nil {
		t.Fatal("expected an error")
	}
	if err.Error() != "clone failed: permission denied" {
		t.Fatalf("error = %q, want it to include the output", err)
	}
	if lines := transcript.Lines(); len(lines) != 1 || lines[0] != "permission denied" {
		t.Fatalf("transcript = %v, want the failure output", lines)
	}
}

func TestRunTrackedFallsBackToExitError(t *testing.T) {
	transcript := logging.NewTranscript()
	err := RunTracked(context.Background(), transcript, "silent failure", "sh", "-c", "exit 3")
	if err == nil || !strings.HasPrefix(err.Error(), "silent failure: ") {
		t.Fatalf("error = %v, want the caller message with the exit status", err)
	}
	if len(transcript.Lines()) != 0 {
		t.Fatalf("transcript = %v, want no lines for silent output", transcript.Lines())
	}
}
