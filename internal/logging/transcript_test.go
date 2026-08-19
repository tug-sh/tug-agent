package logging

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func TestTranscriptCollectsLines(t *testing.T) {
	transcript := NewTranscript("Starting...")
	transcript.Addf("Using image %s", "caddy:2")
	transcript.AddCommandOutput("  first\nsecond  \n")
	transcript.AddCommandOutput("   ")
	transcript.Merge([]string{"nested"})

	lines, err := transcript.Done("Finished.")
	if err != nil {
		t.Fatalf("Done returned an error: %v", err)
	}
	want := []string{"Starting...", "Using image caddy:2", "first", "second", "nested", "Finished."}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines %v, want %d", len(lines), lines, len(want))
	}
	for index := range want {
		if lines[index] != want[index] {
			t.Errorf("line %d = %q, want %q", index, lines[index], want[index])
		}
	}
}

func TestTranscriptFailKeepsLines(t *testing.T) {
	transcript := NewTranscript("Configuring route...")
	lines, err := transcript.Fail("domain %s is taken", "app.example.com")
	if err == nil || err.Error() != "domain app.example.com is taken" {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(lines) != 1 || lines[0] != "Configuring route..." {
		t.Fatalf("expected the transcript to survive the failure, got %v", lines)
	}
}

func TestTranscriptMirrorsToLogger(t *testing.T) {
	var buffer bytes.Buffer
	logger := New(LevelDebug).WithSink(log.New(&buffer, "", 0))

	transcript := NewTranscript().MirrorTo(logger, "[git_deploy] ")
	transcript.Addf("cloning %s", "repo")

	if !strings.Contains(buffer.String(), "[debug] [git_deploy] cloning repo") {
		t.Fatalf("expected the mirrored line, got %q", buffer.String())
	}
}
