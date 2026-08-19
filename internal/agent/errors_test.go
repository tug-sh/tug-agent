package agent

import (
	"errors"
	"fmt"
	"testing"
)

func TestNonRetriableMarker(t *testing.T) {
	cause := errors.New("token revoked")
	marked := markNonRetriable(cause)

	if !isNonRetriable(marked) {
		t.Fatal("expected the marker to be detected")
	}
	if !errors.Is(marked, cause) {
		t.Fatal("expected the cause to stay unwrappable")
	}
	if marked.Error() != cause.Error() {
		t.Fatalf("Error() = %q, want %q", marked.Error(), cause.Error())
	}
	// The marker must survive being wrapped further up the call stack.
	if !isNonRetriable(fmt.Errorf("session failed: %w", marked)) {
		t.Fatal("expected the marker to survive wrapping")
	}
	if markNonRetriable(nil) != nil {
		t.Fatal("marking a nil error must stay nil")
	}
	if isNonRetriable(cause) || isNonRetriable(nil) {
		t.Fatal("plain errors must not be reported as non-retriable")
	}
}

func TestPendingAuthMarker(t *testing.T) {
	cause := errors.New("pairing pending")
	marked := markPendingAuth(cause)

	if !isPendingAuthError(marked) {
		t.Fatal("expected the marker to be detected")
	}
	if !isPendingAuthError(fmt.Errorf("dial failed: %w", marked)) {
		t.Fatal("expected the marker to survive wrapping")
	}
	if isPendingAuthError(markNonRetriable(cause)) {
		t.Fatal("the two markers must stay distinct")
	}
	if markPendingAuth(nil) != nil {
		t.Fatal("marking a nil error must stay nil")
	}
}
