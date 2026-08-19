package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"

	"tug.sh/pkg/protocol"
)

type TerminalSession struct {
	ID        string
	Container string
	Cmd       *exec.Cmd
	PTY       *os.File
	WriteMu   sync.Mutex
	Closed    bool
}

func (runtime *Runtime) handleTerminalCommand(ctx context.Context, conn *websocket.Conn, cmd protocol.Command) error {
	runtime.termMu.Lock()
	defer runtime.termMu.Unlock()

	switch cmd.Type {
	case "terminal_start":
		target := cmd.TargetContainerName
		if target == "" {
			target = cmd.TargetContainerID
		}
		if cmd.TerminalID == "" || target == "" {
			return fmt.Errorf("terminal_id and target_container_name/target_container_id are required")
		}

		if _, exists := runtime.terminals[cmd.TerminalID]; exists {
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
		// Pass TERM=xterm-256color & COLORTERM=truecolor to enable rich ANSI color output.
		shellCmd := exec.Command("docker", "exec", "-it", "-e", "TERM=xterm-256color", "-e", "COLORTERM=truecolor", target, shell)
		ptmx, err := pty.Start(shellCmd)
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
			Cmd:       shellCmd,
			PTY:       ptmx,
		}
		runtime.terminals[cmd.TerminalID] = session

		// Read from PTY in background
		go func() {
			defer func() {
				runtime.termMu.Lock()
				if _, ok := runtime.terminals[cmd.TerminalID]; ok {
					delete(runtime.terminals, cmd.TerminalID)
				}
				runtime.termMu.Unlock()
				ptmx.Close()
				shellCmd.Wait()
			}()

			buf := make([]byte, 4096)
			for {
				n, err := ptmx.Read(buf)
				if err != nil {
					// The PTY closed or the shell exited. Say so, otherwise the
					// browser keeps a dead session open.
					_ = runtime.emitSignal(conn, protocol.EntityTerminal, protocol.ActionOutput, protocol.TerminalOutput{
						TerminalID: cmd.TerminalID,
						Closed:     true,
					})
					return
				}
				if n == 0 {
					continue
				}
				// Terminal bytes have their own message type now. They used to
				// travel as a command result with the terminal id in the
				// command field, which meant every reader had to know the trick.
				output := protocol.TerminalOutput{
					TerminalID: cmd.TerminalID,
					Data:       base64.StdEncoding.EncodeToString(buf[:n]),
				}
				if writeErr := runtime.emitSignal(conn, protocol.EntityTerminal, protocol.ActionOutput, output); writeErr != nil {
					runtime.log.Error("failed to send terminal output: %v", writeErr)
				}
			}
		}()

		return nil

	case "terminal_input":
		session, ok := runtime.terminals[cmd.TerminalID]
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
		session, ok := runtime.terminals[cmd.TerminalID]
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
		session, ok := runtime.terminals[cmd.TerminalID]
		if !ok {
			// Already gone — treat as success so the client can proceed.
			return nil
		}
		delete(runtime.terminals, cmd.TerminalID)
		session.close()
		return nil

	default:
		return fmt.Errorf("unknown terminal command %s", cmd.Type)
	}
}

// close terminates the underlying shell process and PTY. Safe to call multiple times.
func (session *TerminalSession) close() {
	session.WriteMu.Lock()
	defer session.WriteMu.Unlock()
	if session.Closed {
		return
	}
	session.Closed = true
	if session.PTY != nil {
		_ = session.PTY.Close()
	}
	if session.Cmd != nil && session.Cmd.Process != nil {
		_ = session.Cmd.Process.Kill()
	}
}

// closeAllTerminals tears down every active terminal session. Called when the
// agent's websocket connection ends, because each session's output loop is
// bound to that connection — leaving them alive would orphan shell processes
// inside containers and cause duplicated output on reconnect.
func (runtime *Runtime) closeAllTerminals() {
	runtime.termMu.Lock()
	sessions := make([]*TerminalSession, 0, len(runtime.terminals))
	for id, session := range runtime.terminals {
		sessions = append(sessions, session)
		delete(runtime.terminals, id)
	}
	runtime.termMu.Unlock()
	for _, session := range sessions {
		session.close()
	}
}
