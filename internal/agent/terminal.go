package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

type TerminalSession struct {
	ID        string
	Container string
	Cmd       *exec.Cmd
	PTY       *os.File
	WriteMu   sync.Mutex
	Closed    bool
}

func (r *Runtime) handleTerminalCommand(ctx context.Context, conn *websocket.Conn, cmd inboundCommand) error {
	r.termMu.Lock()
	defer r.termMu.Unlock()

	switch cmd.Type {
	case "terminal_start":
		target := cmd.TargetContainerName
		if target == "" {
			target = cmd.TargetContainerID
		}
		if cmd.TerminalID == "" || target == "" {
			return fmt.Errorf("terminal_id and target_container_name/target_container_id are required")
		}
		
		if _, exists := r.terminals[cmd.TerminalID]; exists {
			return fmt.Errorf("terminal session already exists")
		}

		// Try bash first, fall back to sh.
		shell := "/bin/bash"
		checkCmd := exec.Command("docker", "exec", target, "/bin/bash", "-c", "exit 0")
		if checkCmd.Run() != nil {
			shell = "/bin/sh"
		}

		// docker exec -it negotiates TTY raw-mode itself: the docker client puts
		// the host side into raw mode so only the container shell echoes input.
		// pty.Start handles the controlling-terminal setup correctly — do NOT add
		// manual MakeRaw here, or input/output gets echoed twice.
		c := exec.Command("docker", "exec", "-it", target, shell)
		ptmx, err := pty.Start(c)
		if err != nil {
			return fmt.Errorf("failed to start pty: %w", err)
		}

		// Initial resize if provided.
		if cmd.Rows > 0 && cmd.Cols > 0 {
			_ = pty.Setsize(ptmx, &pty.Winsize{Rows: cmd.Rows, Cols: cmd.Cols})
		}

		session := &TerminalSession{
			ID:        cmd.TerminalID,
			Container: target,
			Cmd:       c,
			PTY:       ptmx,
		}
		r.terminals[cmd.TerminalID] = session

		// Read from PTY in background
		go func() {
			defer func() {
				r.termMu.Lock()
				if _, ok := r.terminals[cmd.TerminalID]; ok {
					delete(r.terminals, cmd.TerminalID)
				}
				r.termMu.Unlock()
				ptmx.Close()
				c.Wait()
			}()

			buf := make([]byte, 4096)
			for {
				n, err := ptmx.Read(buf)
				if err != nil {
					// PTY closed or command exited
					return
				}
				if n > 0 {
					payloadBase64 := base64.StdEncoding.EncodeToString(buf[:n])
					outbound := outboundCommandResult{
						Type:      "terminal_output",
						CommandID: cmd.TerminalID, // use CommandID field to route back to specific terminal
						Success:   true,
						Logs:      []string{payloadBase64}, // Send as base64 array element to reuse outboundCommandResult struct
					}
					// Fire and forget
					if writeErr := r.writeJSON(conn, outbound); writeErr != nil {
						log.Printf("failed to send terminal output: %v", writeErr)
					}
				}
			}
		}()
		
		return nil

	case "terminal_input":
		session, ok := r.terminals[cmd.TerminalID]
		if !ok {
			return fmt.Errorf("terminal session not found")
		}
		
		payload, err := base64.StdEncoding.DecodeString(cmd.Payload)
		if err != nil {
			return fmt.Errorf("invalid base64 payload: %w", err)
		}

		session.WriteMu.Lock()
		defer session.WriteMu.Unlock()
		_, err = session.PTY.Write(payload)
		return err

	case "terminal_resize":
		session, ok := r.terminals[cmd.TerminalID]
		if !ok {
			return fmt.Errorf("terminal session not found")
		}

		if cmd.Rows > 0 && cmd.Cols > 0 {
			err := pty.Setsize(session.PTY, &pty.Winsize{
				Rows: cmd.Rows,
				Cols: cmd.Cols,
			})
			if err != nil {
				return fmt.Errorf("failed to resize pty: %w", err)
			}
		}
		return nil

	case "terminal_stop":
		session, ok := r.terminals[cmd.TerminalID]
		if !ok {
			// Already gone — treat as success so the client can proceed.
			return nil
		}
		delete(r.terminals, cmd.TerminalID)
		session.close()
		return nil

	default:
		return fmt.Errorf("unknown terminal command %s", cmd.Type)
	}
}

// close terminates the underlying shell process and PTY. Safe to call multiple times.
func (s *TerminalSession) close() {
	s.WriteMu.Lock()
	defer s.WriteMu.Unlock()
	if s.Closed {
		return
	}
	s.Closed = true
	if s.PTY != nil {
		_ = s.PTY.Close()
	}
	if s.Cmd != nil && s.Cmd.Process != nil {
		_ = s.Cmd.Process.Kill()
	}
}

// closeAllTerminals tears down every active terminal session. Called when the
// agent's websocket connection ends, because each session's output loop is
// bound to that connection — leaving them alive would orphan shell processes
// inside containers and cause duplicated output on reconnect.
func (r *Runtime) closeAllTerminals() {
	r.termMu.Lock()
	sessions := make([]*TerminalSession, 0, len(r.terminals))
	for id, session := range r.terminals {
		sessions = append(sessions, session)
		delete(r.terminals, id)
	}
	r.termMu.Unlock()
	for _, session := range sessions {
		session.close()
	}
}
