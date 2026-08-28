package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"tug.sh/pkg/protocol"
	"tug.sh/services/agent/internal/docker"
	"tug.sh/services/agent/internal/logging"
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
	if commitOut, err := exec.CommandContext(ctx, "git", "-C", deployDir, "log", "-1", "--pretty=format:%h - %s").Output(); err == nil && len(commitOut) > 0 {
		transcript.Addf("Commit: %s", strings.TrimSpace(string(commitOut)))
	}
	syncProjectEnvFile(deployDir, cmd.ProjectID, transcript)

	// Nixpacks builds an image straight from the source tree, no compose or
	// Dockerfile required. The repo is already checked out, so hand off here.
	if strings.EqualFold(strings.TrimSpace(cmd.FileType), "nixpacks") {
		return runtime.deployNixpacks(ctx, cmd, deployDir, transcript)
	}

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

// nixpacksInstallScript is the vendor's one-line installer. It drops the binary
// into /usr/local/bin, which is why nixpacksBinary falls back to that path when
// the agent's own PATH (often trimmed under systemd) does not include it yet.
const nixpacksInstallScript = "curl -fsSL https://nixpacks.com/install.sh | bash"

// deployNixpacks turns a plain source repository into a running container with
// no compose file and no Dockerfile: Nixpacks detects the language, builds an
// image, and a generated compose file starts it. This is the git push -> deploy
// path for developers who do not want to write container config by hand.
func (runtime *Runtime) deployNixpacks(
	ctx context.Context,
	cmd protocol.Command,
	deployDir string,
	transcript *logging.Transcript,
) ([]string, error) {
	if err := ensureNixpacks(ctx, transcript); err != nil {
		writeDeploymentLog(cmd.ProjectID, cmd.CommandID, transcript.Lines(), err)
		return transcript.Lines(), err
	}

	image := nixpacksImageName(cmd.ProjectID)
	transcript.Addf("Building image %s from source with Nixpacks (zero-config)...", image)

	buildCmd := exec.CommandContext(ctx, nixpacksBinary(), "build", ".", "--name", image)
	buildCmd.Dir = deployDir
	buildOutput, buildErr := buildCmd.CombinedOutput()
	transcript.AddCommandOutput(string(buildOutput))
	if buildErr != nil {
		wrapped := fmt.Errorf("nixpacks build failed: %v", buildErr)
		writeDeploymentLog(cmd.ProjectID, cmd.CommandID, transcript.Lines(), wrapped)
		return transcript.Lines(), wrapped
	}

	composeContent := generateNixpacksCompose(ctx, image)
	composePath := filepath.Join(deployDir, defaultComposeFile)
	if writeErr := os.WriteFile(composePath, []byte(composeContent), deployedFileMode); writeErr != nil {
		wrapped := fmt.Errorf("failed to write generated compose file: %w", writeErr)
		writeDeploymentLog(cmd.ProjectID, cmd.CommandID, transcript.Lines(), wrapped)
		return transcript.Lines(), wrapped
	}

	deployCmd, description := docker.DeployCommand(ctx, cmd.Command, "-f", composePath, "up", "-d")
	transcript.Addf("Starting the built image with: %s", description)
	deployCmd.Dir = deployDir

	upOutput, deployErr := deployCmd.CombinedOutput()
	transcript.AddCommandOutput(string(upOutput))
	if deployErr != nil {
		wrapped := fmt.Errorf("compose up failed: %v, output: %s", deployErr, string(upOutput))
		writeDeploymentLog(cmd.ProjectID, cmd.CommandID, transcript.Lines(), wrapped)
		return transcript.Lines(), wrapped
	}

	logs, _ := transcript.Done("Deployment completed successfully.")
	writeDeploymentLog(cmd.ProjectID, cmd.CommandID, logs, nil)
	return logs, nil
}

// ensureNixpacks makes the nixpacks CLI available, installing it via the vendor
// script the first time a repo asks for a zero-config build.
func ensureNixpacks(ctx context.Context, transcript *logging.Transcript) error {
	if _, err := exec.LookPath("nixpacks"); err == nil {
		return nil
	}
	if _, err := os.Stat("/usr/local/bin/nixpacks"); err == nil {
		return nil
	}
	transcript.Addf("Nixpacks not found on this machine. Installing the Nixpacks builder...")
	install := exec.CommandContext(ctx, "sh", "-c", nixpacksInstallScript)
	output, err := install.CombinedOutput()
	transcript.AddCommandOutput(string(output))
	if err != nil {
		return fmt.Errorf("nixpacks install failed: %w", err)
	}
	if _, lookErr := exec.LookPath("nixpacks"); lookErr != nil {
		if _, statErr := os.Stat("/usr/local/bin/nixpacks"); statErr != nil {
			return fmt.Errorf("nixpacks is still not available after install")
		}
	}
	transcript.Addf("Nixpacks installed.")
	return nil
}

// nixpacksBinary resolves the CLI, preferring PATH and falling back to the fixed
// install location the vendor script uses.
func nixpacksBinary() string {
	if path, err := exec.LookPath("nixpacks"); err == nil {
		return path
	}
	return "/usr/local/bin/nixpacks"
}

// nixpacksImageName derives a stable, docker-safe image tag from the project id.
func nixpacksImageName(projectID string) string {
	var builder strings.Builder
	for _, char := range strings.ToLower(strings.TrimSpace(projectID)) {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9', char == '-', char == '_':
			builder.WriteRune(char)
		default:
			builder.WriteRune('-')
		}
	}
	name := strings.Trim(builder.String(), "-_")
	if name == "" {
		name = "app"
	}
	return "tug-nixpacks-" + name
}

// generateNixpacksCompose writes a minimal compose file around the built image.
// If the image declares an exposed port, it is published on the same host port
// so the app is reachable without any manual configuration.
func generateNixpacksCompose(ctx context.Context, image string) string {
	portsBlock := ""
	if port := detectExposedPort(ctx, image); port != "" {
		portsBlock = fmt.Sprintf("    ports:\n      - \"%s:%s\"\n", port, port)
	}
	return fmt.Sprintf("services:\n  app:\n    image: %s\n    restart: always\n%s", image, portsBlock)
}

// detectExposedPort reads the first port the built image EXPOSEs, so the
// generated compose can publish it. An image without an exposed port still runs;
// it simply is not published.
func detectExposedPort(ctx context.Context, image string) string {
	output, err := exec.CommandContext(
		ctx,
		"docker",
		"inspect",
		"--format",
		"{{range $port, $_ := .Config.ExposedPorts}}{{$port}} {{end}}",
		image,
	).Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(output))
	if len(fields) == 0 {
		return ""
	}
	port := fields[0]
	if slash := strings.Index(port, "/"); slash > 0 {
		port = port[:slash]
	}
	return port
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
