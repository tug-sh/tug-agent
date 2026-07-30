package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	// If directory doesn't exist, we clone it. If it does, we pull.
	if _, err := os.Stat(deployDir); os.IsNotExist(err) {
		logFn(fmt.Sprintf("Cloning repo %s (branch %s) into %s...", repoURL, branch, deployDir))
		gitCmd := exec.CommandContext(ctx, "git", "clone", "-b", branch, repoURL, deployDir)
		if output, err := gitCmd.CombinedOutput(); err != nil {
			logFn(fmt.Sprintf("git clone failed: %s", string(output)))
			return logs, fmt.Errorf("git clone failed: %w", err)
		}
		logFn("git clone succeeded.")
	} else {
		logFn(fmt.Sprintf("Directory %s exists. Pulling latest from branch %s...", deployDir, branch))
		
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

	// 3. Execute deployment based on FileType
	if cmd.FileType == "compose" {
		targetFile := cmd.FilePath
		if targetFile == "" {
			targetFile = "docker-compose.yml"
		}
		
		composePath := filepath.Join(deployDir, targetFile)
		if _, err := os.Stat(composePath); os.IsNotExist(err) {
			return logs, fmt.Errorf("compose file %s not found in repo", targetFile)
		}

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
			return logs, fmt.Errorf("compose up failed: %v, output: %s", err, string(output))
		}
		logFn("Deployment completed successfully.")
	} else {
		logFn(fmt.Sprintf("Unsupported file type %s. Skipping deployment command.", cmd.FileType))
	}

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
