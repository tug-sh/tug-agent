package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func (r *Runtime) handleGitDeploy(ctx context.Context, cmd inboundCommand) ([]string, error) {
	if cmd.ProjectID == "" {
		return nil, fmt.Errorf("project_id is required for git_deploy")
	}

	logs := []string{}
	logFn := func(msg string) {
		logs = append(logs, msg)
		r.debugf("[git_deploy] %s", msg)
	}

	logFn(fmt.Sprintf("Starting git_deploy for project %s...", cmd.ProjectID))

	deployDir, err := ResolveSandboxPath(filepath.Join("projects", cmd.ProjectID))
	if err != nil {
		return logs, fmt.Errorf("failed to resolve project sandbox path: %w", err)
	}

	if err := os.MkdirAll(deployDir, 0755); err != nil {
		return logs, fmt.Errorf("failed to create deployments dir: %w", err)
	}

	repoURL := cmd.RepoURL
	if repoURL == "" {
		return logs, fmt.Errorf("repo_url is empty")
	}

	branch := cmd.Branch
	if branch == "" {
		branch = "main" // fallback
	}

	// 1. Fetch code
	// Check if .git subdirectory exists inside deployDir
	gitMetaDir := filepath.Join(deployDir, ".git")
	if _, err := os.Stat(gitMetaDir); os.IsNotExist(err) {
		logFn(fmt.Sprintf("Cloning repo %s (branch %s) into %s...", repoURL, branch, deployDir))
		
		// Remove empty directory if os.MkdirAll created it so git clone can populate it cleanly
		_ = os.RemoveAll(deployDir)

		gitCmd := exec.CommandContext(ctx, "git", "clone", "-b", branch, repoURL, deployDir)
		if output, err := gitCmd.CombinedOutput(); err != nil {
			logFn(fmt.Sprintf("git clone failed: %s", string(output)))
			return logs, fmt.Errorf("git clone failed: %w", err)
		}
		logFn("git clone succeeded.")
	} else {
		logFn(fmt.Sprintf("Repository exists at %s. Pulling latest from branch %s...", deployDir, branch))
		
		// First fetch
		fetchCmd := exec.CommandContext(ctx, "git", "-C", deployDir, "fetch", "origin", branch)
		if output, err := fetchCmd.CombinedOutput(); err != nil {
			logFn(fmt.Sprintf("git fetch failed: %s", string(output)))
			return logs, fmt.Errorf("git fetch failed: %w", err)
		}
		
		// Then reset hard to ensure clean state
		resetCmd := exec.CommandContext(ctx, "git", "-C", deployDir, "reset", "--hard", "origin/"+branch)
		if output, err := resetCmd.CombinedOutput(); err != nil {
			logFn(fmt.Sprintf("git reset failed: %s", string(output)))
			return logs, fmt.Errorf("git reset failed: %w", err)
		}
		logFn("git pull (fetch+reset) succeeded.")
	}

	// 2. Prepare merged .env file (Git repository .env + Dashboard overrides)
	syncProjectEnvFile(deployDir, cmd.ProjectID, logFn)

	// 3. Resolve compose file or Dockerfile
	targetFile := cmd.FilePath
	if targetFile == "" {
		targetFile = "docker-compose.yml"
	}
	composePath := filepath.Join(deployDir, targetFile)

	if _, err := os.Stat(composePath); os.IsNotExist(err) {
		// Fallback checks for common filenames
		if _, errAlt := os.Stat(filepath.Join(deployDir, "docker-compose.yaml")); errAlt == nil {
			targetFile = "docker-compose.yaml"
			composePath = filepath.Join(deployDir, targetFile)
		} else if _, errAlt := os.Stat(filepath.Join(deployDir, "compose.yaml")); errAlt == nil {
			targetFile = "compose.yaml"
			composePath = filepath.Join(deployDir, targetFile)
		} else if _, errAlt := os.Stat(filepath.Join(deployDir, "compose.yml")); errAlt == nil {
			targetFile = "compose.yml"
			composePath = filepath.Join(deployDir, targetFile)
		} else if _, errAlt := os.Stat(filepath.Join(deployDir, "Dockerfile")); errAlt == nil {
			// Auto-generate docker-compose.yml for Dockerfile projects
			logFn("No docker-compose file found, but Dockerfile exists. Auto-generating docker-compose.yml...")
			genComposeContent := "services:\n  app:\n    build:\n      context: .\n      dockerfile: Dockerfile\n    restart: always\n"
			_ = os.WriteFile(filepath.Join(deployDir, "docker-compose.yml"), []byte(genComposeContent), 0644)
			targetFile = "docker-compose.yml"
			composePath = filepath.Join(deployDir, targetFile)
		} else {
			return logs, fmt.Errorf("compose file or Dockerfile '%s' not found in repo after clone", targetFile)
		}
	}

	// Ensure standard projects/<ProjectID>/docker-compose.yml exists for dashboard & API editor
	standardComposePath := filepath.Join(deployDir, "docker-compose.yml")
	if composePath != standardComposePath {
		if content, readErr := os.ReadFile(composePath); readErr == nil {
			_ = os.WriteFile(standardComposePath, content, 0644)
		}
	}

	// 4. Execute deployment
	var dcCmd *exec.Cmd
	if cmd.Command != "" {
		logFn(fmt.Sprintf("Running custom command: %s", cmd.Command))
		dcCmd = exec.CommandContext(ctx, "sh", "-c", cmd.Command)
	} else {
		var composeCommand string
		dcCmd, composeCommand = ComposeCommand(ctx, "-f", composePath, "up", "-d", "--build")
		logFn(fmt.Sprintf("Running %s -f %s up -d --build...", composeCommand, targetFile))
	}
	dcCmd.Dir = deployDir
	
	output, err := dcCmd.CombinedOutput()
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) != "" {
			logFn(line)
		}
	}
	
	if err != nil {
		deployErr := fmt.Errorf("compose up failed: %v, output: %s", err, string(output))
		writeDeploymentLog(cmd.ProjectID, cmd.CommandID, logs, deployErr)
		return logs, deployErr
	}
	logFn("Deployment completed successfully.")
	writeDeploymentLog(cmd.ProjectID, cmd.CommandID, logs, nil)

	return logs, nil
}

func syncProjectEnvFile(deployDir string, projectID string, logFn func(string)) {
	envVars := map[string]string{}

	// 1. Read existing .env from Git repository if present
	repoEnvPath := filepath.Join(deployDir, ".env")
	if data, err := os.ReadFile(repoEnvPath); err == nil {
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") || !strings.Contains(trimmed, "=") {
				continue
			}
			idx := strings.Index(trimmed, "=")
			k := strings.TrimSpace(trimmed[:idx])
			v := strings.TrimSpace(trimmed[idx+1:])
			if k != "" {
				envVars[k] = v
			}
		}
		if len(envVars) > 0 {
			logFn(fmt.Sprintf("Loaded %d base environment variables from repository .env", len(envVars)))
		}
	}

	// 2. Read dashboard overrides from projects/<project_id>/env.json
	dashboardEnvPath, err := ResolveSandboxPath(filepath.Join("projects", projectID, "env.json"))
	if err == nil {
		if data, err := os.ReadFile(dashboardEnvPath); err == nil {
			dashboardVars := map[string]string{}
			if err := json.Unmarshal(data, &dashboardVars); err == nil {
				overrideCount := 0
				for k, v := range dashboardVars {
					if _, exists := envVars[k]; exists {
						overrideCount++
					}
					envVars[k] = v
				}
				if len(dashboardVars) > 0 {
					logFn(fmt.Sprintf("Applied %d environment variables from dashboard (%d overrides)", len(dashboardVars), overrideCount))
				}
			}
		}
	}

	// 3. Write merged .env file to deployDir/.env
	if len(envVars) > 0 {
		var sb strings.Builder
		sb.WriteString("# Generated by Tug.sh (Git .env + Dashboard overrides)\n")
		for k, v := range envVars {
			sb.WriteString(fmt.Sprintf("%s=%s\n", k, v))
		}
		_ = os.WriteFile(repoEnvPath, []byte(sb.String()), 0644)
	}
}

func writeDeploymentLog(projectID string, commandID string, logs []string, deployErr error) {
	if strings.TrimSpace(projectID) == "" {
		return
	}
	logsDir, err := ResolveSandboxPath(filepath.Join("projects", projectID, "logs"))
	if err != nil {
		return
	}
	_ = os.MkdirAll(logsDir, 0755)

	now := time.Now()
	timestampStr := now.Format("20060102-150405")
	logFile := filepath.Join(logsDir, fmt.Sprintf("deploy-%s.log", timestampStr))
	latestLogFile := filepath.Join(logsDir, "latest-deploy.log")

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("=== Deployment Log [%s] ===\n", now.Format("2006-01-02 15:04:05 MST")))
	sb.WriteString(fmt.Sprintf("Project ID : %s\n", projectID))
	if commandID != "" {
		sb.WriteString(fmt.Sprintf("Command ID : %s\n", commandID))
	}
	sb.WriteString("--------------------------------------------------------------------------------\n")
	for _, l := range logs {
		sb.WriteString(l + "\n")
	}
	if deployErr != nil {
		sb.WriteString(fmt.Sprintf("\n[RESULT] FAILED: %v\n", deployErr))
	} else {
		sb.WriteString("\n[RESULT] SUCCESS: Deployment completed successfully.\n")
	}

	content := []byte(sb.String())
	_ = os.WriteFile(logFile, content, 0644)
	_ = os.WriteFile(latestLogFile, content, 0644)
}
