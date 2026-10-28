package importer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"tug.sh/pkg/protocol"
)

// ScanDokploy inspects /etc/dokploy/compose and running Docker containers to build project migration plans.
func ScanDokploy(ctx context.Context, basePath string) ([]protocol.ImportProjectPlan, error) {
	composeDir := filepath.Join(basePath, "compose")
	entries, err := os.ReadDir(composeDir)
	if err != nil {
		// Fallback to checking docker containers if /etc/dokploy/compose doesn't exist
		return ScanDockerCompose(ctx)
	}

	var projects []protocol.ImportProjectPlan

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		appName := entry.Name()
		if strings.EqualFold(appName, "dokploy") || strings.EqualFold(appName, "traefik") {
			continue
		}

		appDir := filepath.Join(composeDir, appName)
		composePath := findComposeFileInDir(appDir)
		if composePath == "" {
			continue
		}

		rawComposeBytes, err := os.ReadFile(composePath)
		if err != nil {
			continue
		}
		rawCompose := string(rawComposeBytes)

		// Read .env if present
		env := make(map[string]string)
		envPath := filepath.Join(appDir, ".env")
		if envBytes, err := os.ReadFile(envPath); err == nil {
			env = ParseEnvFile(string(envBytes))
		}

		// Clean compose and extract Traefik domains
		res := ExtractRoutesAndCleanCompose(rawCompose)

		// Discover running containers belonging to this project
		containers := findContainersForProject(ctx, appName)

		// Also check container labels directly if no routes were extracted from compose
		if len(res.Routes) == 0 && len(containers) > 0 {
			res.Routes = extractRoutesFromContainers(ctx, containers)
		}

		var warnings []string
		if len(res.Routes) == 0 {
			warnings = append(warnings, "No domain routing labels detected; project will run as internal service.")
		}

		plan := protocol.ImportProjectPlan{
			ID:              slugify(appName),
			Name:            appName,
			SourceType:      "compose",
			OriginalCompose: rawCompose,
			CleanedCompose:  res.CleanedYAML,
			Env:             env,
			Domains:         res.Routes,
			Containers:      containers,
			Warnings:        warnings,
		}

		projects = append(projects, plan)
	}

	return projects, nil
}

func findComposeFileInDir(dir string) string {
	candidates := []string{
		"docker-compose.yml",
		"docker-compose.yaml",
		"compose.yml",
		"compose.yaml",
	}
	for _, c := range candidates {
		p := filepath.Join(dir, c)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var out []rune
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			out = append(out, r)
		} else if r == '_' || r == ' ' {
			out = append(out, '-')
		}
	}
	res := strings.Trim(string(out), "-")
	if res == "" {
		return fmt.Sprintf("project-%d", os.Getpid())
	}
	return res
}
