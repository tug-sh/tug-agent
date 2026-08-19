// Package pairing exchanges a code typed by a person for the credential this
// machine will use from then on.
//
// The code is short because somebody has to read it off a screen and type it
// here. That is the whole reason this exchange exists: the token itself is far
// too long to type, and asking anyone to paste it would put it in the shell
// history of the machine and in the clipboard of the laptop.
package pairing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

// Credential is what the API hands back once the code checks out.
type Credential struct {
	ServerID     string `json:"server_id"`
	AgentToken   string `json:"agent_token"`
	WebSocketURL string `json:"ws_url"`
}

// ErrRefused means the code was wrong, expired, or already spent. It is worth
// telling apart from a network failure, because the answer for the person at
// the keyboard is different: type it again versus check the connection.
var ErrRefused = errors.New("the code was not accepted")

type machine struct {
	Code     string `json:"code"`
	HostName string `json:"host_name"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
}

// Claim swaps the code for a credential. The API address comes from the caller
// so a self hosted installation talks to its own control plane.
func Claim(ctx context.Context, apiBaseURL, code string) (Credential, error) {
	hostName, err := os.Hostname()
	if err != nil {
		hostName = ""
	}
	payload, err := json.Marshal(machine{
		Code:     strings.TrimSpace(code),
		HostName: hostName,
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
	})
	if err != nil {
		return Credential{}, fmt.Errorf("cannot encode the pairing request: %w", err)
	}

	endpoint := strings.TrimRight(strings.TrimSpace(apiBaseURL), "/") + "/v1/install/claim"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return Credential{}, fmt.Errorf("cannot build the pairing request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return Credential{}, fmt.Errorf("cannot reach %s: %w", endpoint, err)
	}
	defer response.Body.Close()

	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusTooManyRequests:
		return Credential{}, fmt.Errorf("%w: %s", ErrRefused, reason(response))
	default:
		return Credential{}, fmt.Errorf("the server answered %d: %s", response.StatusCode, reason(response))
	}

	var credential Credential
	if err := json.NewDecoder(response.Body).Decode(&credential); err != nil {
		return Credential{}, fmt.Errorf("cannot read the pairing response: %w", err)
	}
	if credential.ServerID == "" || credential.AgentToken == "" {
		return Credential{}, errors.New("the pairing response was incomplete")
	}
	return credential, nil
}

// reason pulls the human readable half out of an error response, so the person
// at the keyboard is told what to do rather than shown a status code.
func reason(response *http.Response) string {
	var body struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return http.StatusText(response.StatusCode)
	}
	if body.Message != "" {
		return body.Message
	}
	if body.Error != "" {
		return body.Error
	}
	return http.StatusText(response.StatusCode)
}
