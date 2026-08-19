package agent

import (
	"context"
	"testing"
	"time"

	"tug.sh/services/agent/internal/protocol"
)

func TestRequire(t *testing.T) {
	value, err := require("  srv-1 ", "server_id")
	if err != nil || value != "srv-1" {
		t.Fatalf("require() = %q, %v, want the trimmed value", value, err)
	}

	_, err = require("   ", "container_id")
	if err == nil || err.Error() != "container_id is required" {
		t.Fatalf("err = %v, want a container_id requirement", err)
	}
}

func TestCommandRequestRequiredFields(t *testing.T) {
	request := commandRequest{command: protocol.Command{
		ContainerID: " c1 ",
		ProjectID:   " p1 ",
		Domain:      " app.example.com ",
	}}

	if got, err := request.requireContainerID(); err != nil || got != "c1" {
		t.Errorf("requireContainerID() = %q, %v", got, err)
	}
	if got, err := request.requireProjectID(); err != nil || got != "p1" {
		t.Errorf("requireProjectID() = %q, %v", got, err)
	}
	if got, err := request.requireDomain(); err != nil || got != "app.example.com" {
		t.Errorf("requireDomain() = %q, %v", got, err)
	}

	empty := commandRequest{}
	for name, call := range map[string]func() (string, error){
		"container_id": empty.requireContainerID,
		"project_id":   empty.requireProjectID,
		"domain":       empty.requireDomain,
	} {
		if _, err := call(); err == nil {
			t.Errorf("expected a missing %s to fail", name)
		}
	}
}

func TestCommandRequestWithTimeout(t *testing.T) {
	request := commandRequest{ctx: context.Background()}

	timeoutCtx, cancel := request.withTimeout(50 * time.Millisecond)
	defer cancel()

	if _, hasDeadline := timeoutCtx.Deadline(); !hasDeadline {
		t.Fatal("expected the derived context to carry a deadline")
	}
	select {
	case <-timeoutCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("the derived context did not expire")
	}
}

// A handler that cannot reach its parent context must still get a usable one.
func TestCommandRequestWithTimeoutInheritsCancellation(t *testing.T) {
	parentCtx, cancelParent := context.WithCancel(context.Background())
	request := commandRequest{ctx: parentCtx}

	timeoutCtx, cancel := request.withTimeout(time.Minute)
	defer cancel()

	cancelParent()
	select {
	case <-timeoutCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("cancelling the parent must cancel the command context")
	}
}
