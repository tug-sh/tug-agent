package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
	"golang.org/x/term"
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

		// Open PTY pair explicitly so we can configure the slave before the
		// child starts. pty.Start only exposes the master, and calling
		// term.MakeRaw on the master has no effect on macOS because the
		// master has no line discipline — it must be called on the slave.
		ptmx, pts, openErr := pty.Open()
		if openErr != nil {
			return fmt.Errorf("failed to open pty: %w", openErr)
		}

		// Initial size.
		if cmd.Rows > 0 && cmd.Cols > 0 {
			_ = pty.Setsize(ptmx, &pty.Winsize{Rows: cmd.Rows, Cols: cmd.Cols})
		}

		// Disable echo on the slave side BEFORE the child inherits it.
		// The container shell (via docker exec -t) echoes characters itself
		// through readline; the host PTY must not add a second echo.
		if _, rawErr := term.MakeRaw(int(pts.Fd())); rawErr != nil {
			log.Printf("terminal: could not disable pty echo: %v", rawErr)
		}

		c := exec.Command("docker", "exec", "-it", target, shell)
		c.Stdin = pts
		c.Stdout = pts
		c.Stderr = pts
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}

		if err := c.Start(); err != nil {
			_ = ptmx.Close()
			_ = pts.Close()
			return fmt.Errorf("failed to start pty: %w", err)
		}
		// Slave is now owned by the child; close the parent's copy.
		_ = pts.Close()

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

	default:
		return fmt.Errorf("unknown terminal command %s", cmd.Type)
	}
}
