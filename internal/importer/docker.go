package importer

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"tug.sh/pkg/protocol"
)

// ScanDockerCompose discovers all active Compose stacks directly from the local Docker daemon.
func ScanDockerCompose(ctx context.Context) ([]protocol.ImportProjectPlan, error) {
	cmd := exec.CommandContext(ctx, "docker", "ps", "-a", "--format", "{{.ID}}\t{{.Names}}\t{{.Label \"com.docker.compose.project\"}}\t{{.Label \"com.docker.compose.project.working_dir\"}}\t{{.Label \"com.docker.compose.project.config_files\"}}")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to list docker containers: %w", err)
	}

	type stackMeta struct {
		projectName string
		workingDir  string
		configFiles string
		containers  []string
	}

	stacks := make(map[string]*stackMeta)

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 3 {
			continue
		}
		name := strings.TrimSpace(parts[1])
		project := strings.TrimSpace(parts[2])
		workingDir := ""
		if len(parts) > 3 {
			workingDir = strings.TrimSpace(parts[3])
		}
		configFiles := ""
		if len(parts) > 4 {
			configFiles = strings.TrimSpace(parts[4])
		}

		// Skip internal system and control containers
		if shouldIgnoreProject(project, name) {
			continue
		}

		if project == "" {
			// Container not part of an explicit compose project; skip or group
			continue
		}

		s, ok := stacks[project]
		if !ok {
			s = &stackMeta{
				projectName: project,
				workingDir:  workingDir,
				configFiles: configFiles,
			}
			stacks[project] = s
		}
		s.containers = append(s.containers, name)
		if s.workingDir == "" && workingDir != "" {
			s.workingDir = workingDir
		}
		if s.configFiles == "" && configFiles != "" {
			s.configFiles = configFiles
		}
	}

	var result []protocol.ImportProjectPlan

	for projName, meta := range stacks {
		rawCompose := ""
		env := make(map[string]string)

		// 1. Try to read compose file from config_files or working_dir
		composePath := resolveComposePath(meta.configFiles, meta.workingDir)
		if composePath != "" {
			if content, err := os.ReadFile(composePath); err == nil {
				rawCompose = string(content)
			}
		}

		// 2. Read .env file if available in working_dir
		if meta.workingDir != "" {
			envPath := filepath.Join(meta.workingDir, ".env")
			if envBytes, err := os.ReadFile(envPath); err == nil {
				env = ParseEnvFile(string(envBytes))
			}
		}

		cleaned := ExtractRoutesAndCleanCompose(rawCompose)

		// If no routes found in compose, check container labels directly
		if len(cleaned.Routes) == 0 && len(meta.containers) > 0 {
			cleaned.Routes = extractRoutesFromContainers(ctx, meta.containers)
		}

		// If compose was completely missing on disk, build fallback compose specification
		if strings.TrimSpace(rawCompose) == "" {
			rawCompose = generateFallbackCompose(ctx, meta.containers)
			cleaned.CleanedYAML = rawCompose
		}

		var warnings []string
		if len(cleaned.Routes) == 0 {
			warnings = append(warnings, "No domain routing labels detected for this project.")
		}

		result = append(result, protocol.ImportProjectPlan{
			ID:              slugify(projName),
			Name:            projName,
			SourceType:      "compose",
			OriginalCompose: rawCompose,
			CleanedCompose:  cleaned.CleanedYAML,
			Env:             env,
			Domains:         cleaned.Routes,
			Containers:      meta.containers,
			Warnings:        warnings,
		})
	}

	return result, nil
}

func shouldIgnoreProject(project, name string) bool {
	p := strings.ToLower(project)
	n := strings.ToLower(name)
	ignores := []string{
		"tug-agent", "tug-router", "tug-metrics",
		"dokploy-traefik", "dokploy-postgres",
		"coolify-proxy", "coolify-db", "coolify-redis", "coolify-realtime",
	}
	for _, ign := range ignores {
		if strings.Contains(p, ign) || strings.Contains(n, ign) {
			return true
		}
	}
	return false
}

func resolveComposePath(configFiles, workingDir string) string {
	if configFiles != "" {
		for _, f := range strings.Split(configFiles, ",") {
			f = strings.TrimSpace(f)
			if f != "" {
				if fi, err := os.Stat(f); err == nil && !fi.IsDir() {
					return f
				}
			}
		}
	}
	if workingDir != "" {
		return findComposeFileInDir(workingDir)
	}
	return ""
}

func findContainersForProject(ctx context.Context, projectName string) []string {
	cmd := exec.CommandContext(ctx, "docker", "ps", "-a", "--filter", "label=com.docker.compose.project="+projectName, "--format", "{{.Names}}")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var res []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		l = strings.TrimSpace(l)
		if l != "" {
			res = append(res, l)
		}
	}
	return res
}

func extractRoutesFromContainers(ctx context.Context, containerNames []string) []protocol.ImportDomainRoute {
	var routes []protocol.ImportDomainRoute
	seen := make(map[string]bool)

	for _, name := range containerNames {
		cmd := exec.CommandContext(ctx, "docker", "inspect", name, "--format", "{{json .Config.Labels}}")
		out, err := cmd.Output()
		if err != nil {
			continue
		}

		rawLabels := string(out)
		// Search for Host(`...`) in labels
		matches := hostRuleRe.FindAllStringSubmatch(rawLabels, -1)
		for _, m := range matches {
			if len(m) > 1 {
				for _, dm := range domainExtractRe.FindAllStringSubmatch(m[1], -1) {
					if len(dm) > 1 {
						dom := strings.ToLower(strings.TrimSpace(dm[1]))
						if dom != "" && !seen[dom] {
							seen[dom] = true
							routes = append(routes, protocol.ImportDomainRoute{
								Domain:        dom,
								TargetService: name,
								HTTPS:         true,
							})
						}
					}
				}
			}
		}

		// Look for port label
		if pm := portLabelRe.FindStringSubmatch(rawLabels); len(pm) > 1 {
			if port, err := strconv.Atoi(pm[1]); err == nil {
				for i := range routes {
					if routes[i].TargetService == name && routes[i].TargetPort == 0 {
						routes[i].TargetPort = port
					}
				}
			}
		}
	}

	return routes
}

func generateFallbackCompose(ctx context.Context, containerNames []string) string {
	var sb strings.Builder
	sb.WriteString("version: '3.8'\nservices:\n")
	for _, name := range containerNames {
		cmd := exec.CommandContext(ctx, "docker", "inspect", name, "--format", "{{.Config.Image}}")
		imageBytes, _ := cmd.Output()
		image := strings.TrimSpace(string(imageBytes))
		if image == "" {
			image = "unknown:latest"
		}
		sb.WriteString(fmt.Sprintf("  %s:\n    image: %s\n    restart: unless-stopped\n", slugify(name), image))
	}
	return sb.String()
}
