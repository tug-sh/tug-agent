package docker

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"tug.sh/services/agent/internal/logging"
	"tug.sh/services/agent/internal/sandbox"
	"tug.sh/services/agent/internal/shell"
)

// ComposeCommand builds a compose invocation, preferring the docker CLI plugin
// and falling back to a standalone docker-compose binary on older hosts.
func ComposeCommand(ctx context.Context, args ...string) (*exec.Cmd, string) {
	pluginCheck := exec.CommandContext(ctx, "docker", "compose", "version")
	if pluginCheck.Run() == nil {
		return exec.CommandContext(ctx, "docker", append([]string{"compose"}, args...)...), "docker compose"
	}
	if path, err := exec.LookPath("docker-compose"); err == nil {
		return exec.CommandContext(ctx, path, args...), "docker-compose"
	}
	return exec.CommandContext(ctx, "docker", append([]string{"compose"}, args...)...), "docker compose"
}

func (manager *Manager) ComposeUp(ctx context.Context, appName string) error {
	targetPath, err := sandbox.ResolvePath(appName)
	if err != nil {
		return err
	}
	cmd, composeCommand := ComposeCommand(ctx, "-f", filepath.Join(targetPath, "docker-compose.yml"), "up", "-d")
	if output, runErr := cmd.CombinedOutput(); runErr != nil {
		return fmt.Errorf("%s up failed: %s: %w", composeCommand, string(output), runErr)
	}
	return nil
}

// DeployCommand builds the process that materializes a project: the command the
// project defines, or a compose invocation when it defines none. The returned
// description is the command line to show in the deployment transcript.
func DeployCommand(ctx context.Context, customCommand string, composeArgs ...string) (*exec.Cmd, string) {
	if trimmed := strings.TrimSpace(customCommand); trimmed != "" {
		return shell.Custom(ctx, trimmed), trimmed
	}
	cmd, composeName := ComposeCommand(ctx, composeArgs...)
	return cmd, composeName + " " + strings.Join(composeArgs, " ")
}

// DeployCompose runs the compose file, or a custom command when the project
// overrides it, and returns the transcript shown in the dashboard.
func (manager *Manager) DeployCompose(ctx context.Context, composePath string, customCommand string) ([]string, error) {
	projectDir := filepath.Dir(composePath)
	transcript := logging.NewTranscript(fmt.Sprintf("Running deployment in %s...", projectDir))

	cmd, description := DeployCommand(ctx, customCommand, "-f", composePath, "up", "-d")
	transcript.Addf("Command: %s", description)
	cmd.Dir = projectDir

	output, err := cmd.CombinedOutput()
	outputText := strings.TrimSpace(string(output))
	transcript.AddCommandOutput(outputText)
	if err != nil {
		return transcript.Fail("compose deployment failed: %v, output: %s", err, outputText)
	}
	return transcript.Done("Deployment finished successfully.")
}
