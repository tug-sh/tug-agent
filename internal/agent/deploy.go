package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tug.sh/services/agent/internal/docker"
	"tug.sh/services/agent/internal/logging"
	"tug.sh/services/agent/internal/protocol"
	"tug.sh/services/agent/internal/sandbox"
	"tug.sh/services/agent/internal/shell"
)

const (
	defaultDeployBranch  = "main"
	defaultComposeFile   = "docker-compose.yml"
	deployedFileMode     = 0o644
	deployedDirMode      = 0o755
	gitDeployLogPrefix   = "[git_deploy] "
	dockerfileComposeGen = "services:\n  app:\n    build:\n      context: .\n      dockerfile: Dockerfile\n    restart: always\n"
)

// alternativeComposeFiles are checked when the requested compose file is absent,
// covering the naming variants commonly found in repositories.
var alternativeComposeFiles = []string{"docker-compose.yaml", "compose.yaml", "compose.yml"}

func (runtime *Runtime) handleGitDeploy(ctx context.Context, cmd protocol.Command) ([]string, error) {
	if cmd.ProjectID == "" {
		return nil, errors.New("project_id is required for git_deploy")
	}
	transcript := logging.NewTranscript().MirrorTo(runtime.log, gitDeployLogPrefix)
	transcript.Addf("Starting git_deploy for project %s...", cmd.ProjectID)

	deployDir, err := sandbox.ResolvePath(filepath.Join("projects", cmd.ProjectID))
	if err != nil {
		return transcript.Fail("failed to resolve project sandbox path: %w", err)
	}
	if mkdirErr := os.MkdirAll(deployDir, deployedDirMode); mkdirErr != nil {
		return transcript.Fail("failed to create deployments dir: %w", mkdirErr)
	}
	repoURL := strings.TrimSpace(cmd.RepoURL)
	if repoURL == "" {
		return transcript.Fail("repo_url is empty")
	}
	branch := cmd.Branch
	if branch == "" {
		branch = defaultDeployBranch
	}

	if fetchErr := fetchRepository(ctx, transcript, deployDir, repoURL, branch); fetchErr != nil {
		return transcript.Lines(), fetchErr
	}
	syncProjectEnvFile(deployDir, cmd.ProjectID, transcript)

	composePath, composeFile, resolveErr := resolveComposeFile(deployDir, cmd.FilePath, transcript)
	if resolveErr != nil {
		return transcript.Lines(), resolveErr
	}
	mirrorStandardComposeFile(deployDir, composePath)

	deployCmd, description := docker.DeployCommand(ctx, cmd.Command, "-f", composePath, "up", "-d", "--build")
	transcript.Addf("Deploying %s with: %s", composeFile, description)
	deployCmd.Dir = deployDir

	output, deployErr := deployCmd.CombinedOutput()
	transcript.AddCommandOutput(string(output))
	if deployErr != nil {
		wrapped := fmt.Errorf("compose up failed: %v, output: %s", deployErr, string(output))
		writeDeploymentLog(cmd.ProjectID, cmd.CommandID, transcript.Lines(), wrapped)
		return transcript.Lines(), wrapped
	}
	logs, _ := transcript.Done("Deployment completed successfully.")
	writeDeploymentLog(cmd.ProjectID, cmd.CommandID, logs, nil)
	return logs, nil
}

func fetchRepository(
	ctx context.Context,
	transcript *logging.Transcript,
	deployDir string,
	repoURL string,
	branch string,
) error {
	gitMetaDir := filepath.Join(deployDir, ".git")
	if _, err := os.Stat(gitMetaDir); os.IsNotExist(err) {
		return cloneRepository(ctx, transcript, deployDir, repoURL, branch)
	}
	return pullRepository(ctx, transcript, deployDir, branch)
}

func cloneRepository(
	ctx context.Context,
	transcript *logging.Transcript,
	deployDir string,
	repoURL string,
	branch string,
) error {
	transcript.Addf("Cloning repo %s (branch %s) into %s...", repoURL, branch, deployDir)
	// git clone needs to create the directory itself, so drop the empty one
	// created while preparing the sandbox.
	_ = os.RemoveAll(deployDir)

	if err := shell.RunTracked(ctx, transcript, "git clone failed", "git", "clone", "-b", branch, repoURL, deployDir); err != nil {
		return err
	}
	transcript.Addf("git clone succeeded.")
	return nil
}

// pullRepository refreshes an existing checkout with fetch plus a hard reset,
// so local drift on the VPS never blocks a deployment.
func pullRepository(ctx context.Context, transcript *logging.Transcript, deployDir string, branch string) error {
	transcript.Addf("Repository exists at %s. Pulling latest from branch %s...", deployDir, branch)

	if err := shell.RunTracked(ctx, transcript, "git fetch failed", "git", "-C", deployDir, "fetch", "origin", branch); err != nil {
		return err
	}
	if err := shell.RunTracked(ctx, transcript, "git reset failed", "git", "-C", deployDir, "reset", "--hard", "origin/"+branch); err != nil {
		return err
	}
	transcript.Addf("git pull (fetch+reset) succeeded.")
	return nil
}

// resolveComposeFile locates the compose file to deploy, falling back to the
// common alternative names and finally to a generated file for Dockerfile-only
// repositories. It returns the absolute path and the file name.
func resolveComposeFile(
	deployDir string,
	requestedFile string,
	transcript *logging.Transcript,
) (string, string, error) {
	requested := requestedFile
	if requested == "" {
		requested = defaultComposeFile
	}
	candidates := append([]string{requested}, alternativeComposeFiles...)
	for _, candidate := range candidates {
		candidatePath := filepath.Join(deployDir, candidate)
		if _, err := os.Stat(candidatePath); err == nil {
			return candidatePath, candidate, nil
		}
	}

	if _, err := os.Stat(filepath.Join(deployDir, "Dockerfile")); err != nil {
		return "", "", fmt.Errorf("compose file or Dockerfile '%s' not found in repo after clone", requested)
	}
	transcript.Addf("No docker-compose file found, but Dockerfile exists. Auto-generating docker-compose.yml...")
	generatedPath := filepath.Join(deployDir, defaultComposeFile)
	_ = os.WriteFile(generatedPath, []byte(dockerfileComposeGen), deployedFileMode)
	return generatedPath, defaultComposeFile, nil
}

// mirrorStandardComposeFile keeps projects/<id>/docker-compose.yml in sync with
// the deployed file, because the dashboard editor reads that fixed path.
func mirrorStandardComposeFile(deployDir string, composePath string) {
	standardComposePath := filepath.Join(deployDir, defaultComposeFile)
	if composePath == standardComposePath {
		return
	}
	content, readErr := os.ReadFile(composePath)
	if readErr != nil {
		return
	}
	_ = os.WriteFile(standardComposePath, content, deployedFileMode)
}

// syncProjectEnvFile merges the repository .env with the dashboard overrides and
// writes the result back, so compose sees a single authoritative env file.
func syncProjectEnvFile(deployDir string, projectID string, transcript *logging.Transcript) {
	repoEnvPath := filepath.Join(deployDir, ".env")
	envVars := readEnvFile(repoEnvPath)
	if len(envVars) > 0 {
		transcript.Addf("Loaded %d base environment variables from repository .env", len(envVars))
	}

	dashboardVars := readDashboardEnvOverrides(projectID)
	if len(dashboardVars) > 0 {
		overrideCount := 0
		for key, value := range dashboardVars {
			if _, exists := envVars[key]; exists {
				overrideCount++
			}
			envVars[key] = value
		}
		transcript.Addf(
			"Applied %d environment variables from dashboard (%d overrides)",
			len(dashboardVars),
			overrideCount,
		)
	}

	if len(envVars) == 0 {
		return
	}
	var builder strings.Builder
	builder.WriteString("# Generated by Tug.sh (Git .env + Dashboard overrides)\n")
	for key, value := range envVars {
		builder.WriteString(fmt.Sprintf("%s=%s\n", key, value))
	}
	_ = os.WriteFile(repoEnvPath, []byte(builder.String()), deployedFileMode)
}

func readEnvFile(path string) map[string]string {
	envVars := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return envVars
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		separatorIndex := strings.Index(trimmed, "=")
		if separatorIndex <= 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:separatorIndex])
		if key == "" {
			continue
		}
		envVars[key] = strings.TrimSpace(trimmed[separatorIndex+1:])
	}
	return envVars
}

func readDashboardEnvOverrides(projectID string) map[string]string {
	overrides := map[string]string{}
	dashboardEnvPath, err := sandbox.ResolvePath(filepath.Join("projects", projectID, "env.json"))
	if err != nil {
		return overrides
	}
	data, readErr := os.ReadFile(dashboardEnvPath)
	if readErr != nil {
		return overrides
	}
	if unmarshalErr := json.Unmarshal(data, &overrides); unmarshalErr != nil {
		return map[string]string{}
	}
	return overrides
}

func writeDeploymentLog(projectID string, commandID string, logs []string, deployErr error) {
	if strings.TrimSpace(projectID) == "" {
		return
	}
	logsDir, err := sandbox.ResolvePath(filepath.Join("projects", projectID, "logs"))
	if err != nil {
		return
	}
	_ = os.MkdirAll(logsDir, deployedDirMode)

	now := time.Now()
	logFile := filepath.Join(logsDir, fmt.Sprintf("deploy-%s.log", now.Format("20060102-150405")))
	latestLogFile := filepath.Join(logsDir, "latest-deploy.log")

	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("=== Deployment Log [%s] ===\n", now.Format("2006-01-02 15:04:05 MST")))
	builder.WriteString(fmt.Sprintf("Project ID : %s\n", projectID))
	if commandID != "" {
		builder.WriteString(fmt.Sprintf("Command ID : %s\n", commandID))
	}
	builder.WriteString(strings.Repeat("-", 80) + "\n")
	for _, line := range logs {
		builder.WriteString(line + "\n")
	}
	if deployErr != nil {
		builder.WriteString(fmt.Sprintf("\n[RESULT] FAILED: %v\n", deployErr))
	} else {
		builder.WriteString("\n[RESULT] SUCCESS: Deployment completed successfully.\n")
	}

	content := []byte(builder.String())
	_ = os.WriteFile(logFile, content, deployedFileMode)
	_ = os.WriteFile(latestLogFile, content, deployedFileMode)
}
