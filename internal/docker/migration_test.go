package docker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// fakeExecCommand returns a command that runs the TestHelperProcess function.
func fakeExecCommand(ctx context.Context, command string, args ...string) *exec.Cmd {
	cs := []string{"-test.run=TestHelperProcess", "--", command}
	cs = append(cs, args...)
	cmd := exec.CommandContext(ctx, os.Args[0], cs...)
	// Set an environment variable so the test helper knows to take over
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
	return cmd
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 {
		if args[0] == "--" {
			args = args[1:]
			break
		}
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "No command\n")
		os.Exit(2)
	}
	cmd, cmdArgs := args[0], args[1:]

	switch cmd {
	case "docker":
		if len(cmdArgs) > 0 && cmdArgs[0] == "inspect" {
			// Fail inspect for specific container
			if strings.Contains(strings.Join(cmdArgs, " "), "fail_inspect") {
				os.Exit(1)
			}
			fmt.Fprintf(os.Stdout, "/my_test_container\n")
			os.Exit(0)
		}
		if len(cmdArgs) > 0 && cmdArgs[0] == "commit" {
			if strings.Contains(strings.Join(cmdArgs, " "), "fail_commit") {
				os.Exit(1)
			}
			os.Exit(0)
		}
		if len(cmdArgs) > 0 && cmdArgs[0] == "save" {
			if strings.Contains(strings.Join(cmdArgs, " "), "fail_save") {
				os.Exit(1)
			}
			fmt.Fprintf(os.Stdout, "fake tar data")
			os.Exit(0)
		}
		os.Exit(0)
	case "ssh":
		if strings.Contains(strings.Join(cmdArgs, " "), "fail_ssh") {
			os.Exit(1)
		}
		os.Exit(0)
	}

	os.Exit(0)
}

func TestMigrateContainerToTarget_Success(t *testing.T) {
	oldExec := execCommandContext
	execCommandContext = fakeExecCommand
	defer func() { execCommandContext = oldExec }()

	mgr := NewManager()
	ctx := context.Background()

	err := mgr.MigrateContainerToTarget(ctx, "test_container", "1.2.3.4", 2222, "private-key", false, nil)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
}

func TestMigrateContainerToTarget_FailInspect(t *testing.T) {
	oldExec := execCommandContext
	execCommandContext = fakeExecCommand
	defer func() { execCommandContext = oldExec }()

	mgr := NewManager()
	ctx := context.Background()

	err := mgr.MigrateContainerToTarget(ctx, "fail_inspect", "1.2.3.4", 2222, "private-key", false, nil)
	if err == nil {
		t.Fatalf("expected failure from inspect, got success")
	}
	if !strings.Contains(err.Error(), "failed to inspect container") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestMigrateContainerToTarget_FailCommit(t *testing.T) {
	oldExec := execCommandContext
	execCommandContext = fakeExecCommand
	defer func() { execCommandContext = oldExec }()

	mgr := NewManager()
	ctx := context.Background()

	err := mgr.MigrateContainerToTarget(ctx, "fail_commit", "1.2.3.4", 2222, "private-key", false, nil)
	if err == nil {
		t.Fatalf("expected failure from commit, got success")
	}
	if !strings.Contains(err.Error(), "failed to commit container image") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestMigrateContainerToTarget_FailSSH(t *testing.T) {
	oldExec := execCommandContext
	execCommandContext = fakeExecCommand
	defer func() { execCommandContext = oldExec }()

	mgr := NewManager()
	ctx := context.Background()

	err := mgr.MigrateContainerToTarget(ctx, "test_container", "fail_ssh", 2222, "private-key", false, nil)
	if err == nil {
		t.Fatalf("expected failure from ssh, got success")
	}
	if !strings.Contains(err.Error(), "failed to execute target ssh") && !strings.Contains(err.Error(), "exit status 1") {
		t.Errorf("unexpected error message: %v", err)
	}
}
