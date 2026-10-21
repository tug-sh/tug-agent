package docker

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

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


func (manager *Manager) RedeployContainer(ctx context.Context, containerID string) ([]string, error) {
	return manager.RedeployContainerWithReport(ctx, containerID, nil)
}

func (manager *Manager) RedeployContainerWithReport(ctx context.Context, containerID string, report func([]string)) ([]string, error) {
	containerID = strings.TrimSpace(containerID)
	startMsg := fmt.Sprintf("Preparing redeploy for container %s...", containerID)
	transcript := logging.NewTranscript(startMsg)
	if report != nil {
		report([]string{startMsg})
	}
	if containerID == "" {
		return transcript.Fail("container_id is required")
	}

	project := ComposeLabel(ctx, containerID, "com.docker.compose.project")
	workingDir := ComposeLabel(ctx, containerID, "com.docker.compose.project.working_dir")
	configFiles := ComposeLabel(ctx, containerID, "com.docker.compose.project.config_files")

	if workingDir == "" && project != "" {
		if sbPath, err := sandbox.ResolvePath(filepath.Join("projects", project)); err == nil {
			if _, statErr := os.Stat(filepath.Join(sbPath, "docker-compose.yml")); statErr == nil {
				workingDir = sbPath
			}
		}
	}

	if project == "" || workingDir == "" {
		transcript.Addf("Inspecting container %s labels...", containerID)
		if project == "" {
			transcript.Addf("Missing label com.docker.compose.project on container %s", containerID)
		}
		if workingDir == "" {
			transcript.Addf("Missing label com.docker.compose.project.working_dir on container %s", containerID)
		}
		return transcript.Fail("this container was not deployed with Docker Compose, so it cannot be redeployed from here")
	}

	files := splitComposeConfigFiles(configFiles)
	if len(files) == 0 {
		files = []string{filepath.Join(workingDir, "docker-compose.yml")}
	}

	args := []string{"-p", project}
	for _, file := range files {
		if !filepath.IsAbs(file) {
			file = filepath.Join(workingDir, file)
		}
		args = append(args, "-f", file)
	}
	args = append(args, "up", "-d")

	redeployMsg := fmt.Sprintf("Redeploying compose project %s in %s...", project, workingDir)
	transcript.Add(redeployMsg)
	if report != nil {
		report([]string{redeployMsg})
	}
	cmd, composeName := ComposeCommand(ctx, args...)
	cmd.Dir = workingDir
	cmdMsg := fmt.Sprintf("Command: %s %s", composeName, strings.Join(args, " "))
	transcript.Add(cmdMsg)
	if report != nil {
		report([]string{cmdMsg})
	}

	err := RunStreamingCommand(cmd, transcript, report)
	if err != nil {
		return transcript.Fail("compose up failed: %v", err)
	}
	return transcript.Done("Redeploy finished successfully.")
}

// RunStreamingCommand executes cmd, streaming stdout and stderr line-by-line
// to the transcript and report callback as lines are emitted.
func RunStreamingCommand(cmd *exec.Cmd, transcript *logging.Transcript, report func([]string)) error {
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	done := make(chan struct{})
	lineChan := make(chan string, 200)

	go func() {
		defer close(done)
		var pending []string
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()

		flush := func() {
			if len(pending) == 0 {
				return
			}
			batch := pending
			pending = nil
			if report != nil {
				report(batch)
			}
		}

		for {
			select {
			case line, ok := <-lineChan:
				if !ok {
					flush()
					return
				}
				if transcript != nil {
					transcript.Add(line)
				}
				pending = append(pending, line)
				if len(pending) >= 15 {
					flush()
				}
			case <-ticker.C:
				flush()
			}
		}
	}()

	go func() {
		defer close(lineChan)
		scanner := bufio.NewScanner(pr)
		buf := make([]byte, 64*1024)
		scanner.Buffer(buf, 1024*1024)
		for scanner.Scan() {
			line := strings.TrimRight(scanner.Text(), "\r")
			lineChan <- line
		}
	}()

	err := cmd.Run()
	_ = pw.Close()
	<-done
	_ = pr.Close()
	return err
}

// ComposeLabel reads a single label off a container, returning "" when the
// label is absent so callers can fall back cleanly.
func ComposeLabel(ctx context.Context, containerID, label string) string {
	template := fmt.Sprintf("{{ index .Config.Labels %q }}", label)
	value, err := output(ctx, "inspect", "--format", template, containerID)
	if err != nil {
		return ""
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "<no value>" {
		return ""
	}
	return trimmed
}

// splitComposeConfigFiles parses the config_files label, which Docker writes as
// a comma-separated list of the compose files that built the project.
func splitComposeConfigFiles(raw string) []string {
	files := make([]string, 0, 2)
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			files = append(files, trimmed)
		}
	}
	return files
}

// DeployCompose runs the compose file, or a custom command when the project
// overrides it, and returns the transcript shown in the dashboard.
func (manager *Manager) DeployCompose(ctx context.Context, composePath string, customCommand string) ([]string, error) {
	return manager.DeployComposeWithReport(ctx, composePath, customCommand, nil)
}

// DeployComposeWithReport runs the compose file with live line-by-line streaming.
func (manager *Manager) DeployComposeWithReport(
	ctx context.Context,
	composePath string,
	customCommand string,
	report func([]string),
) ([]string, error) {
	projectDir := filepath.Dir(composePath)
	startMsg := fmt.Sprintf("Running deployment in %s...", projectDir)
	transcript := logging.NewTranscript(startMsg)
	if report != nil {
		report([]string{startMsg})
	}

	cmd, description := DeployCommand(ctx, customCommand, "-f", composePath, "up", "-d")
	cmdMsg := fmt.Sprintf("Command: %s", description)
	transcript.Add(cmdMsg)
	if report != nil {
		report([]string{cmdMsg})
	}
	cmd.Dir = projectDir

	err := RunStreamingCommand(cmd, transcript, report)
	if err != nil {
		return transcript.Fail("compose deployment failed: %v", err)
	}
	return transcript.Done("Deployment finished successfully.")
}
