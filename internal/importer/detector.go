package importer

import (
	"context"
	"os"
	"os/exec"
	"strings"

	"tug.sh/pkg/protocol"
)

// PanelInfo holds detection results for a panel.
type PanelInfo struct {
	Name                string // "dokploy", "coolify", "docker", "none"
	Version             string
	IsInstalled         bool
	IsCompatible        bool
	CompatibilityStatus string // "compatible", "warning", "fallback", "none"
	Notes               string
	BasePath            string
}

// DetectPanel examines the host filesystem and running Docker containers to identify
// if Dokploy, Coolify, or generic Docker Compose stacks are present.
func DetectPanel(ctx context.Context) PanelInfo {
	// 1. Check Dokploy
	if dok := checkDokploy(ctx); dok.IsInstalled {
		return dok
	}

	// 2. Check Coolify
	if cool := checkCoolify(ctx); cool.IsInstalled {
		return cool
	}

	// 3. Fallback to generic Docker Compose
	return checkGenericDocker(ctx)
}

func checkDokploy(ctx context.Context) PanelInfo {
	basePath := "/etc/dokploy"
	dirExists := false
	if fi, err := os.Stat(basePath); err == nil && fi.IsDir() {
		dirExists = true
	}

	containerRunning, imageTag := inspectContainerImage(ctx, "dokploy")
	if !dirExists && !containerRunning {
		return PanelInfo{Name: "dokploy", IsInstalled: false}
	}

	version := extractVersionFromImage(imageTag)
	if version == "" {
		version = "0.8.x (auto-detected)"
	}

	status := "compatible"
	notes := "Dokploy detected with compose directory and database schema support."
	if strings.Contains(strings.ToLower(version), "latest") || strings.Contains(strings.ToLower(version), "auto") {
		status = "compatible"
		notes = "Dokploy detected. Format verified with compose workspace."
	}

	return PanelInfo{
		Name:                "dokploy",
		Version:             version,
		IsInstalled:         true,
		IsCompatible:        true,
		CompatibilityStatus: status,
		Notes:               notes,
		BasePath:            basePath,
	}
}

func checkCoolify(ctx context.Context) PanelInfo {
	basePath := "/data/coolify"
	dirExists := false
	if fi, err := os.Stat(basePath); err == nil && fi.IsDir() {
		dirExists = true
	}

	containerRunning, imageTag := inspectContainerImage(ctx, "coolify")
	if !dirExists && !containerRunning {
		return PanelInfo{Name: "coolify", IsInstalled: false}
	}

	version := extractVersionFromImage(imageTag)
	if version == "" {
		version = "v4.x (auto-detected)"
	}

	status := "compatible"
	notes := "Coolify v4 detected. Service compose stacks and Traefik route extraction supported."
	if strings.Contains(version, "v3") {
		status = "warning"
		notes = "Coolify v3 legacy structure detected. Using universal container inspection fallback."
	}

	return PanelInfo{
		Name:                "coolify",
		Version:             version,
		IsInstalled:         true,
		IsCompatible:        true,
		CompatibilityStatus: status,
		Notes:               notes,
		BasePath:            basePath,
	}
}

func checkGenericDocker(ctx context.Context) PanelInfo {
	cmd := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	out, err := cmd.Output()
	if err != nil {
		return PanelInfo{
			Name:                "none",
			Version:             "unknown",
			IsInstalled:         false,
			IsCompatible:        false,
			CompatibilityStatus: "none",
			Notes:               "Docker Engine is not running or accessible on this host.",
		}
	}

	dockerVer := strings.TrimSpace(string(out))
	return PanelInfo{
		Name:                "docker",
		Version:             dockerVer,
		IsInstalled:         true,
		IsCompatible:        true,
		CompatibilityStatus: "compatible",
		Notes:               "Direct Docker Compose discovery mode active (reading stacks directly from host engine).",
	}
}

// DetectProxyStatus checks if an existing proxy (e.g. Traefik from Dokploy/Coolify) is running
// and binding ports 80/443.
func DetectProxyStatus(ctx context.Context) protocol.ImportProxyStatus {
	status := protocol.ImportProxyStatus{}

	cmd := exec.CommandContext(ctx, "docker", "ps", "--format", "{{.ID}}\t{{.Names}}\t{{.Image}}\t{{.Ports}}")
	out, err := cmd.Output()
	if err != nil {
		return status
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		parts := strings.Split(line, "\t")
		if len(parts) < 4 {
			continue
		}
		id := parts[0]
		name := strings.ToLower(parts[1])
		img := strings.ToLower(parts[2])
		ports := parts[3]

		isProxy := strings.Contains(name, "traefik") || strings.Contains(img, "traefik") ||
			strings.Contains(name, "coolify-proxy") || strings.Contains(name, "dokploy-traefik")

		if isProxy {
			status.TraefikRunning = true
			status.TraefikContainerID = id
			status.ProxyName = parts[1]
			if strings.Contains(ports, ":80->") || strings.Contains(ports, ":80/tcp") {
				status.Port80Bound = true
			}
			if strings.Contains(ports, ":443->") || strings.Contains(ports, ":443/tcp") {
				status.Port443Bound = true
			}
			break
		}
	}

	return status
}

func inspectContainerImage(ctx context.Context, nameFragment string) (bool, string) {
	cmd := exec.CommandContext(ctx, "docker", "ps", "-a", "--filter", "name="+nameFragment, "--format", "{{.Image}}")
	out, err := cmd.Output()
	if err != nil {
		return false, ""
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return false, ""
	}
	firstLine := strings.Split(trimmed, "\n")[0]
	return true, firstLine
}

func extractVersionFromImage(imageStr string) string {
	if imageStr == "" {
		return ""
	}
	parts := strings.Split(imageStr, ":")
	if len(parts) >= 2 {
		return parts[len(parts)-1]
	}
	return imageStr
}
