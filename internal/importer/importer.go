package importer

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"tug.sh/pkg/protocol"
	"tug.sh/services/agent/internal/docker"
	"tug.sh/services/agent/internal/router"
	"tug.sh/services/agent/internal/sandbox"
)

type Manager struct {
	docker *docker.Manager
	router *router.Router
}

func New(dockerManager *docker.Manager, routerManager *router.Router) *Manager {
	return &Manager{
		docker: dockerManager,
		router: routerManager,
	}
}

// Scan identifies existing panels (Dokploy, Coolify, or Docker Compose), extracts
// workloads, strips proxy labels, and produces an actionable migration plan.
func (m *Manager) Scan(ctx context.Context, sourcePreference string) (*protocol.ImportScanResult, error) {
	pref := strings.ToLower(strings.TrimSpace(sourcePreference))

	var panel PanelInfo
	switch pref {
	case "dokploy":
		panel = checkDokploy(ctx)
		if !panel.IsInstalled {
			panel.Notes = "Dokploy directory not found; falling back to Docker compose scanner."
		}
	case "coolify":
		panel = checkCoolify(ctx)
		if !panel.IsInstalled {
			panel.Notes = "Coolify directory not found; falling back to Docker compose scanner."
		}
	case "docker":
		panel = checkGenericDocker(ctx)
	default:
		panel = DetectPanel(ctx)
	}

	var projects []protocol.ImportProjectPlan
	var scanErr error

	switch panel.Name {
	case "dokploy":
		projects, scanErr = ScanDokploy(ctx, panel.BasePath)
	case "coolify":
		projects, scanErr = ScanCoolify(ctx, panel.BasePath)
	default:
		projects, scanErr = ScanDockerCompose(ctx)
	}

	if scanErr != nil {
		return nil, fmt.Errorf("failed to scan %s workloads: %w", panel.Name, scanErr)
	}

	proxyStatus := DetectProxyStatus(ctx)

	return &protocol.ImportScanResult{
		DetectedSource:      panel.Name,
		DetectedVersion:     panel.Version,
		IsCompatible:        panel.IsCompatible,
		CompatibilityStatus: panel.CompatibilityStatus,
		CompatibilityNotes:  panel.Notes,
		Projects:            projects,
		ProxyStatus:         proxyStatus,
	}, nil
}

// Apply materializes the user-approved migration plan by writing compose & env files
// to the Tug apps sandbox and registering Caddy routes.
func (m *Manager) Apply(ctx context.Context, req protocol.ImportApplyRequest) (*protocol.ImportApplyResult, error) {
	result := &protocol.ImportApplyResult{
		ImportedProjects: []string{},
		ConfiguredRoutes: []string{},
	}

	// 1. Optionally disable legacy third-party proxy (e.g. Dokploy/Coolify Traefik)
	if req.DisableOldProxy {
		proxyStatus := DetectProxyStatus(ctx)
		if proxyStatus.TraefikRunning && proxyStatus.TraefikContainerID != "" {
			stopCmd := exec.CommandContext(ctx, "docker", "stop", proxyStatus.TraefikContainerID)
			if stopOut, stopErr := stopCmd.CombinedOutput(); stopErr != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("failed to stop old proxy: %s (%v)", string(stopOut), stopErr))
			} else {
				result.StoppedProxy = true
			}
		}
	}

	// 2. Materialize each project into sandbox
	for _, proj := range req.Projects {
		cleanName := slugify(proj.Name)
		if cleanName == "" {
			cleanName = "app"
		}

		targetPath, err := sandbox.ResolvePath(filepath.Join("projects", cleanName))
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("cannot resolve sandbox path for %s: %v", proj.Name, err))
			continue
		}

		if err := os.MkdirAll(targetPath, 0755); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("cannot create project directory %s: %v", targetPath, err))
			continue
		}

		// Write docker-compose.yml
		composeFilePath := filepath.Join(targetPath, "docker-compose.yml")
		if strings.TrimSpace(proj.Compose) != "" {
			if err := os.WriteFile(composeFilePath, []byte(proj.Compose), 0644); err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("cannot write compose file for %s: %v", proj.Name, err))
				continue
			}
		}

		// Write .env
		if len(proj.Env) > 0 {
			var envLines []string
			var keys []string
			for k := range proj.Env {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				v := proj.Env[k]
				// Quote values containing whitespace or special characters
				if strings.ContainsAny(v, " \t\n\"'$") {
					v = fmt.Sprintf("%q", v)
				}
				envLines = append(envLines, fmt.Sprintf("%s=%s", k, v))
			}
			envFilePath := filepath.Join(targetPath, ".env")
			if err := os.WriteFile(envFilePath, []byte(strings.Join(envLines, "\n")+"\n"), 0600); err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("cannot write .env file for %s: %v", proj.Name, err))
			}
		}

		// Configure routes in TugRouter if active
		if m.router != nil {
			for _, r := range proj.Domains {
				if r.Domain != "" {
					targetPort := r.TargetPort
					if targetPort <= 0 {
						targetPort = 80
					}
					targetName := r.TargetService
					if targetName == "" {
						targetName = cleanName
					}

					_, err := m.router.ConfigureRoute(ctx, router.RouteRequest{
						Domain:              r.Domain,
						TargetContainerName: targetName,
						TargetPort:          targetPort,
					})
					if err != nil {
						result.Errors = append(result.Errors, fmt.Sprintf("cannot configure route %s: %v", r.Domain, err))
					} else {
						result.ConfiguredRoutes = append(result.ConfiguredRoutes, r.Domain)
					}
				}
			}
		}

		result.ImportedProjects = append(result.ImportedProjects, proj.Name)
	}

	return result, nil
}
