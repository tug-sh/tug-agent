package importer

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"tug.sh/pkg/protocol"
)

// ScanCoolify inspects /data/coolify/services and running Coolify containers to construct project plans.
func ScanCoolify(ctx context.Context, basePath string) ([]protocol.ImportProjectPlan, error) {
	var projects []protocol.ImportProjectPlan
	processedNames := make(map[string]bool)

	// 1. Scan /data/coolify/services directory
	servicesDir := filepath.Join(basePath, "services")
	if entries, err := os.ReadDir(servicesDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}

			serviceDir := filepath.Join(servicesDir, entry.Name())
			composePath := findComposeFileInDir(serviceDir)
			if composePath == "" {
				continue
			}

			rawBytes, err := os.ReadFile(composePath)
			if err != nil {
				continue
			}
			rawCompose := string(rawBytes)

			env := make(map[string]string)
			envPath := filepath.Join(serviceDir, ".env")
			if envBytes, err := os.ReadFile(envPath); err == nil {
				env = ParseEnvFile(string(envBytes))
			}

			cleaned := ExtractRoutesAndCleanCompose(rawCompose)
			name := entry.Name()
			if friendlyName, ok := env["COOLIFY_SERVICE_NAME"]; ok && friendlyName != "" {
				name = friendlyName
			}

			containers := findContainersForProject(ctx, name)
			if len(cleaned.Routes) == 0 && len(containers) > 0 {
				cleaned.Routes = extractRoutesFromContainers(ctx, containers)
			}

			var warnings []string
			if len(cleaned.Routes) == 0 {
				warnings = append(warnings, "No domain routing labels found for this service.")
			}

			projects = append(projects, protocol.ImportProjectPlan{
				ID:              slugify(name),
				Name:            name,
				SourceType:      "compose",
				OriginalCompose: rawCompose,
				CleanedCompose:  cleaned.CleanedYAML,
				Env:             env,
				Domains:         cleaned.Routes,
				Containers:      containers,
				Warnings:        warnings,
			})
			processedNames[slugify(name)] = true
		}
	}

	// 2. Discover running containers managed by Coolify (e.g. applications built from git or nixpacks)
	dockerProjects, err := ScanDockerCompose(ctx)
	if err == nil {
		for _, dp := range dockerProjects {
			if processedNames[dp.ID] {
				continue
			}
			// Only include if managed by coolify or if it has coolify labels/paths
			isCoolifyStack := false
			for _, c := range dp.Containers {
				if strings.Contains(strings.ToLower(c), "coolify") {
					isCoolifyStack = true
					break
				}
			}
			if !isCoolifyStack && strings.Contains(dp.OriginalCompose, "coolify") {
				isCoolifyStack = true
			}

			if isCoolifyStack {
				projects = append(projects, dp)
				processedNames[dp.ID] = true
			}
		}
	}

	// If no specific Coolify projects found in /data/coolify, fallback to full Docker compose scanner
	if len(projects) == 0 {
		return ScanDockerCompose(ctx)
	}

	return projects, nil
}
