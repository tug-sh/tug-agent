package agent

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestRunEveryStopsWhenWorkIsDone(t *testing.T) {
	ticks := 0
	runEvery(context.Background(), time.Millisecond, func() bool {
		ticks++
		return ticks < 3
	})
	if ticks != 3 {
		t.Fatalf("ticks = %d, want the loop to stop on the third tick", ticks)
	}
}

func TestRunEveryStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		runEvery(ctx, time.Millisecond, func() bool { return true })
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the loop ignored its cancelled context")
	}
}

func TestHandleControlFrame(t *testing.T) {
	runtime := newTestRuntime(t)

	cases := []struct {
		name        string
		frame       string
		wantHandled bool
		wantErr     bool
	}{
		{"keepalive", `{"type":"keepalive"}`, true, false},
		{"auth error", `{"type":"auth_error","error":"unknown token"}`, true, true},
		{"command", `{"type":"exec_command","command_id":"c1"}`, false, false},
		{"malformed", `not json`, false, false},
	}
	for _, testCase := range cases {
		handled, err := runtime.handleControlFrame([]byte(testCase.frame))
		if handled != testCase.wantHandled {
			t.Errorf("%s: handled = %v, want %v", testCase.name, handled, testCase.wantHandled)
		}
		if (err != nil) != testCase.wantErr {
			t.Errorf("%s: err = %v, want error: %v", testCase.name, err, testCase.wantErr)
		}
	}
}

// An auth_error means the dashboard pairing is incomplete, so the session must
// end with a pending-auth marker and retry quickly instead of backing off.
func TestHandleControlFrameMarksPendingAuth(t *testing.T) {
	runtime := newTestRuntime(t)

	_, err := runtime.handleControlFrame([]byte(`{"type":"auth_error","error":"unknown token"}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !isPendingAuthError(err) {
		t.Fatalf("expected a pending auth marker, got %v", err)
	}
}

func TestIsAuthRejection(t *testing.T) {
	rejected := []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden}
	for _, status := range rejected {
		if !isAuthRejection(&http.Response{StatusCode: status}) {
			t.Errorf("status %d should be treated as an auth rejection", status)
		}
	}
	if isAuthRejection(&http.Response{StatusCode: http.StatusBadGateway}) {
		t.Error("a gateway error is retriable, not an auth rejection")
	}
	if isAuthRejection(nil) {
		t.Error("a missing response must not be treated as an auth rejection")
	}
}

func TestDialAPIRequiresCredentials(t *testing.T) {
	runtime := newTestRuntime(t)
	runtime.config.APIWebSocketURL = "ws://127.0.0.1:1"

	runtime.config.ServerID = ""
	if _, err := runtime.dialAPI(t.Context()); err == nil {
		t.Error("expected an error for a missing server id")
	}

	runtime.config.ServerID = "srv-1"
	runtime.config.AgentToken = ""
	if _, err := runtime.dialAPI(t.Context()); err == nil {
		t.Error("expected an error for a missing token")
	}
}
