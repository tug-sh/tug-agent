package agent

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type DockerManager struct{}

const defaultTugRouterImage = "caddy:2"

type tugRouterRoute struct {
	Domain            string `json:"domain"`
	Target            string `json:"target"`
	Port              int    `json:"port"`
	TargetContainerID string `json:"target_container_id,omitempty"`
}

func tugRouterRoutesPath(routerName string) string {
	return filepath.Join(GetDataDir(), "tug-router", "routes.json")
}

func tugRouterCaddyfilePath(routerName string) string {
	return filepath.Join("/tmp", fmt.Sprintf("%s-Caddyfile", routerName))
}

func NewDockerManager() *DockerManager {
	return &DockerManager{}
}

func (d *DockerManager) EnsureTugNetwork(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "docker", "network", "inspect", "tug-network")
	if err := cmd.Run(); err == nil {
		return nil
	}

	createCmd := exec.CommandContext(ctx, "docker", "network", "create", "tug-network")
	if output, err := createCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cannot create tug-network: %s: %w", string(output), err)
	}
	return nil
}

func (d *DockerManager) ComposeUp(ctx context.Context, appName string) error {
	targetPath, err := ResolveSandboxPath(appName)
	if err != nil {
		return err
	}
	cmd, composeCommand := ComposeCommand(ctx, "-f", targetPath+"/docker-compose.yml", "up", "-d")
	if output, runErr := cmd.CombinedOutput(); runErr != nil {
		return fmt.Errorf("%s up failed: %s: %w", composeCommand, string(output), runErr)
	}
	return nil
}

func (d *DockerManager) StreamEvents(ctx context.Context, onEvent func(raw []byte) error) error {
	cmd := exec.CommandContext(ctx, "docker", "events", "--format", "{{json .}}")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer cmd.Process.Kill()

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Bytes()
		payload := make([]byte, len(line))
		copy(payload, line)
		if err := onEvent(payload); err != nil {
			return err
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

func (d *DockerManager) ListContainers(ctx context.Context) ([]HandshakeContainer, error) {
	return d.listContainers(ctx, true)
}

func (d *DockerManager) ListContainersLite(ctx context.Context) ([]HandshakeContainer, error) {
	return d.listContainers(ctx, false)
}

func (d *DockerManager) listContainers(ctx context.Context, includeLogsPreview bool) ([]HandshakeContainer, error) {
	cmd := exec.CommandContext(
		ctx,
		"docker",
		"ps",
		"-a",
		"--format",
		"{{.ID}}\t{{.Names}}\t{{.Image}}\t{{.Ports}}\t{{.Status}}\t{{.Networks}}\t{{.Label \"com.docker.compose.project\"}}\t{{.Label \"tug.app\"}}",
	)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 1 && strings.TrimSpace(lines[0]) == "" {
		return []HandshakeContainer{}, nil
	}

	containers := make([]HandshakeContainer, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 8 {
			continue
		}
		containerID := strings.TrimSpace(parts[0])
		logsPreview := []string{}
		if includeLogsPreview {
			logsPreview, _ = d.GetLogsPreview(ctx, containerID, 20)
		}
		containers = append(containers, HandshakeContainer{
			ID:          containerID,
			Name:        strings.TrimSpace(parts[1]),
			Image:       strings.TrimSpace(parts[2]),
			Ports:       strings.TrimSpace(parts[3]),
			Status:      normalizeContainerStatus(parts[4]),
			Networks:    splitCSV(parts[5]),
			ProjectID:   strings.TrimSpace(parts[6]),
			App:         strings.TrimSpace(parts[7]),
			LogsPreview: logsPreview,
		})
	}

	return containers, nil
}

func (d *DockerManager) ListNetworks(ctx context.Context) ([]string, error) {
	cmd := exec.CommandContext(
		ctx,
		"docker",
		"network",
		"ls",
		"--format",
		"{{.Name}}",
	)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		result = append(result, name)
	}
	return result, nil
}

func (d *DockerManager) ControlContainer(
	ctx context.Context,
	containerID string,
	action string,
	removeVolumes bool,
	removeImage bool,
) error {
	containerID = strings.TrimSpace(containerID)
	action = strings.TrimSpace(strings.ToLower(action))
	if containerID == "" {
		return fmt.Errorf("container_id is required")
	}
	if action != "start" && action != "stop" && action != "restart" && action != "remove" {
		return fmt.Errorf("unsupported action %s", action)
	}

	var imageName string
	if action == "remove" && removeImage {
		out, err := exec.CommandContext(ctx, "docker", "inspect", "--format={{.Config.Image}}", containerID).Output()
		if err == nil {
			imageName = strings.TrimSpace(string(out))
		}
	}

	args := []string{action, containerID}
	if action == "remove" {
		args = []string{"rm", "-f"}
		if removeVolumes {
			args = append(args, "-v")
		}
		args = append(args, containerID)
	}

	cmd := exec.CommandContext(ctx, "docker", args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker %s failed: %s: %w", action, string(output), err)
	}

	if action == "remove" && removeImage && imageName != "" {
		_ = exec.CommandContext(ctx, "docker", "rmi", imageName).Run()
	}

	return nil
}

func (d *DockerManager) DeployCompose(ctx context.Context, composePath string, customCommand string) ([]string, error) {
	logLines := []string{
		fmt.Sprintf("Running deployment in %s...", filepath.Dir(composePath)),
	}
	var cmd *exec.Cmd
	sandboxDir := filepath.Dir(composePath)

	if customCommand != "" {
		logLines = append(logLines, fmt.Sprintf("Command: %s", customCommand))
		cmd = exec.CommandContext(ctx, "sh", "-c", customCommand)
	} else {
		var composeCommand string
		cmd, composeCommand = ComposeCommand(ctx, "-f", composePath, "up", "-d")
		logLines = append(logLines, fmt.Sprintf("Command: %s -f %s up -d", composeCommand, composePath))
	}
	cmd.Dir = sandboxDir

	output, err := cmd.CombinedOutput()
	outputText := strings.TrimSpace(string(output))
	if outputText != "" {
		logLines = append(logLines, strings.Split(outputText, "\n")...)
	}
	if err != nil {
		return logLines, fmt.Errorf("compose deployment failed: %v, output: %s", err, outputText)
	}
	logLines = append(logLines, "Deployment finished successfully.")
	return logLines, nil
}

func (d *DockerManager) InstallTugRouter(
	ctx context.Context,
	routerName string,
	routerImage string,
) ([]string, error) {
	logLines := []string{
		"Preparing TugRouter installation...",
	}
	resolvedImage := strings.TrimSpace(routerImage)
	if resolvedImage == "" {
		resolvedImage = defaultTugRouterImage
	}
	existingRouterName, resolveErr := d.ResolveTugRouterContainerName(ctx)
	if resolveErr == nil {
		logLines = append(logLines, fmt.Sprintf("Removing previous TugRouter container %s...", existingRouterName))
		removeCmd := exec.CommandContext(ctx, "docker", "rm", "-f", existingRouterName)
		removeOutput, removeErr := removeCmd.CombinedOutput()
		removeOutputText := strings.TrimSpace(string(removeOutput))
		if removeOutputText != "" {
			logLines = append(logLines, removeOutputText)
		}
		if removeErr != nil {
			return logLines, fmt.Errorf("cannot remove previous tug-router container: %w", removeErr)
		}
	} else {
		logLines = append(logLines, "No previous TugRouter container found.")
	}

	logLines = append(logLines, fmt.Sprintf("Starting TugRouter container %s...", routerName))
	logLines = append(logLines, fmt.Sprintf("Using image %s", resolvedImage))
	runCmd := exec.CommandContext(
		ctx,
		"docker",
		"run",
		"-d",
		"--name",
		routerName,
		"--label",
		"tug.app=tug-router",
		"--label",
		"tug.managed=true",
		"--restart",
		"unless-stopped",
		"-p",
		"80:80",
		"-p",
		"443:443",
		resolvedImage,
	)
	runOutput, runErr := runCmd.CombinedOutput()
	runOutputText := strings.TrimSpace(string(runOutput))
	if runOutputText != "" {
		logLines = append(logLines, runOutputText)
	}
	if runErr != nil {
		return logLines, fmt.Errorf("cannot start tug-router container: %w", runErr)
	}
	logLines = append(logLines, "TugRouter is installed and running.")
	return logLines, nil
}

func (d *DockerManager) ResolveTugRouterContainerName(ctx context.Context) (string, error) {
	name, err := d.resolveContainerNameByLabel(ctx, true, "tug.app=tug-router")
	if err == nil {
		return name, nil
	}
	return d.resolveContainerNameByLabel(ctx, false, "tug.app=tug-router")
}

func (d *DockerManager) GenerateTugRouterContainerName() (string, error) {
	buffer := make([]byte, 4)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return fmt.Sprintf("tug-router-%s", strings.ToLower(hex.EncodeToString(buffer))), nil
}

func (d *DockerManager) resolveContainerNameByLabel(
	ctx context.Context,
	runningOnly bool,
	label string,
) (string, error) {
	args := []string{"ps"}
	if !runningOnly {
		args = append(args, "-a")
	}
	args = append(args, "--filter", "label="+label, "--format", "{{.Names}}")
	cmd := exec.CommandContext(ctx, "docker", args...)
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, line := range lines {
		name := strings.TrimSpace(line)
		if name != "" {
			return name, nil
		}
	}
	return "", fmt.Errorf("container with label %s not found", label)
}

func (d *DockerManager) ConfigureTugRouterRoute(
	ctx context.Context,
	routerName string,
	domain string,
	targetContainerID string,
	targetContainerName string,
	targetPort int,
) ([]string, error) {
	normalizedDomain := strings.ToLower(strings.TrimSpace(domain))
	normalizedTargetName := strings.TrimSpace(targetContainerName)
	normalizedTargetID := strings.TrimSpace(targetContainerID)
	logLines := []string{
		"Configuring TugRouter route...",
		fmt.Sprintf("Domain: %s", normalizedDomain),
		fmt.Sprintf("Target: %s:%d", normalizedTargetName, targetPort),
	}

	routes, err := d.ListTugRouterRoutes(routerName)
	if err != nil {
		return logLines, err
	}

	found := false
	for index := range routes {
		if strings.EqualFold(routes[index].Domain, normalizedDomain) {
			existingTargetID := strings.TrimSpace(routes[index].TargetContainerID)
			existingTargetName := strings.TrimSpace(routes[index].Target)
			sameContainerByID := existingTargetID != "" && normalizedTargetID != "" && existingTargetID == normalizedTargetID
			sameContainerByName := strings.EqualFold(existingTargetName, normalizedTargetName)
			if !sameContainerByID && !sameContainerByName {
				return logLines, fmt.Errorf("domain %s is already assigned to container %s", normalizedDomain, existingTargetName)
			}
			routes[index] = tugRouterRoute{
				Domain:            normalizedDomain,
				Target:            normalizedTargetName,
				Port:              targetPort,
				TargetContainerID: normalizedTargetID,
			}
			found = true
			break
		}
	}
	if !found {
		routes = append(routes, tugRouterRoute{
			Domain:            normalizedDomain,
			Target:            normalizedTargetName,
			Port:              targetPort,
			TargetContainerID: normalizedTargetID,
		})
	}

	applyLogs, applyErr := d.applyTugRouterRoutes(ctx, routerName, routes)
	logLines = append(logLines, applyLogs...)
	if applyErr != nil {
		return logLines, applyErr
	}
	logLines = append(logLines, "Route applied successfully.")
	return logLines, nil
}

func (d *DockerManager) ListTugRouterRoutes(routerName string) ([]tugRouterRoute, error) {
	routesPath := tugRouterRoutesPath(routerName)
	legacyRoutesPath := filepath.Join("/tmp", fmt.Sprintf("%s-routes.json", routerName))
	raw, err := os.ReadFile(routesPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		legacyRaw, legacyErr := os.ReadFile(legacyRoutesPath)
		if legacyErr != nil {
			if os.IsNotExist(legacyErr) {
				return []tugRouterRoute{}, nil
			}
			return nil, legacyErr
		}
		if len(legacyRaw) == 0 {
			return []tugRouterRoute{}, nil
		}
		var legacyRoutes []tugRouterRoute
		if unmarshalErr := json.Unmarshal(legacyRaw, &legacyRoutes); unmarshalErr != nil {
			return nil, unmarshalErr
		}
		_ = d.persistTugRouterRoutes(legacyRoutes)
		return legacyRoutes, nil
	}
	if len(raw) == 0 {
		return []tugRouterRoute{}, nil
	}
	var routes []tugRouterRoute
	if unmarshalErr := json.Unmarshal(raw, &routes); unmarshalErr != nil {
		return nil, unmarshalErr
	}
	return routes, nil
}

func (d *DockerManager) DeleteTugRouterRoute(
	ctx context.Context,
	routerName string,
	domain string,
) ([]string, error) {
	trimmedDomain := strings.TrimSpace(domain)
	logLines := []string{
		"Removing TugRouter route...",
		fmt.Sprintf("Domain: %s", trimmedDomain),
	}
	if trimmedDomain == "" {
		return logLines, fmt.Errorf("domain is required")
	}
	routes, err := d.ListTugRouterRoutes(routerName)
	if err != nil {
		return logLines, err
	}
	filtered := make([]tugRouterRoute, 0, len(routes))
	removed := false
	for _, route := range routes {
		if strings.EqualFold(strings.TrimSpace(route.Domain), trimmedDomain) {
			removed = true
			continue
		}
		filtered = append(filtered, route)
	}
	if !removed {
		return logLines, fmt.Errorf("route for domain %s not found", trimmedDomain)
	}
	applyLogs, applyErr := d.applyTugRouterRoutes(ctx, routerName, filtered)
	logLines = append(logLines, applyLogs...)
	if applyErr != nil {
		return logLines, applyErr
	}
	logLines = append(logLines, "Route removed successfully.")
	return logLines, nil
}

func (d *DockerManager) applyTugRouterRoutes(
	ctx context.Context,
	routerName string,
	routes []tugRouterRoute,
) ([]string, error) {
	logLines := make([]string, 0, 3)
	if persistErr := d.persistTugRouterRoutes(routes); persistErr != nil {
		return logLines, persistErr
	}

	var builder strings.Builder
	if len(routes) == 0 {
		builder.WriteString(":80 {\n")
		builder.WriteString("\trespond \"tug-router is running\" 200\n")
		builder.WriteString("}\n")
	} else {
		for _, route := range routes {
			builder.WriteString(strings.TrimSpace(route.Domain))
			builder.WriteString(" {\n")
			builder.WriteString("\treverse_proxy ")
			builder.WriteString(strings.TrimSpace(route.Target))
			builder.WriteString(":")
			builder.WriteString(fmt.Sprintf("%d", route.Port))
			builder.WriteString("\n}\n\n")
		}
	}
	if writeErr := os.WriteFile(tugRouterCaddyfilePath(routerName), []byte(builder.String()), 0o644); writeErr != nil {
		return logLines, writeErr
	}

	copyCmd := exec.CommandContext(
		ctx,
		"docker",
		"cp",
		tugRouterCaddyfilePath(routerName),
		fmt.Sprintf("%s:/etc/caddy/Caddyfile", routerName),
	)
	if copyOutput, copyErr := copyCmd.CombinedOutput(); copyErr != nil {
		return logLines, fmt.Errorf("cannot upload Caddyfile: %s: %w", string(copyOutput), copyErr)
	}
	logLines = append(logLines, "Caddyfile updated.")

	reloadCmd := exec.CommandContext(
		ctx,
		"docker",
		"exec",
		routerName,
		"caddy",
		"reload",
		"--config",
		"/etc/caddy/Caddyfile",
	)
	if reloadOutput, reloadErr := reloadCmd.CombinedOutput(); reloadErr != nil {
		return logLines, fmt.Errorf("cannot reload Caddy config: %s: %w", string(reloadOutput), reloadErr)
	}
	logLines = append(logLines, "Caddy config reloaded.")
	return logLines, nil
}

func (d *DockerManager) persistTugRouterRoutes(routes []tugRouterRoute) error {
	routesRaw, marshalErr := json.MarshalIndent(routes, "", "  ")
	if marshalErr != nil {
		return marshalErr
	}
	path := tugRouterRoutesPath("")
	if mkdirErr := os.MkdirAll(filepath.Dir(path), 0o700); mkdirErr != nil {
		return mkdirErr
	}
	if writeErr := os.WriteFile(path, routesRaw, 0o600); writeErr != nil {
		return writeErr
	}
	return nil
}

func (d *DockerManager) RestartDockerDaemon(ctx context.Context) ([]string, error) {
	logLines := []string{"Restarting Docker daemon..."}
	cmd := exec.CommandContext(ctx, "systemctl", "restart", "docker")
	output, err := cmd.CombinedOutput()
	outputText := strings.TrimSpace(string(output))
	if outputText != "" {
		logLines = append(logLines, outputText)
	}
	if err != nil {
		return logLines, fmt.Errorf("cannot restart docker daemon: %w", err)
	}
	logLines = append(logLines, "Docker daemon restarted.")
	return logLines, nil
}

func (d *DockerManager) ScheduleServerReset(ctx context.Context) ([]string, error) {
	logLines := []string{
		"Scheduling VPS reset...",
		"Server reboot will start in a few seconds.",
	}
	cmd := exec.CommandContext(
		ctx,
		"sh",
		"-c",
		"nohup sh -c 'sleep 3; systemctl reboot' >/tmp/tug-reset.log 2>&1 &",
	)
	output, err := cmd.CombinedOutput()
	outputText := strings.TrimSpace(string(output))
	if outputText != "" {
		logLines = append(logLines, outputText)
	}
	if err != nil {
		return logLines, fmt.Errorf("cannot schedule server reboot: %w", err)
	}
	logLines = append(logLines, "Reset scheduled.")
	return logLines, nil
}

func ComposeCommand(ctx context.Context, args ...string) (*exec.Cmd, string) {
	composeV2Check := exec.CommandContext(ctx, "docker", "compose", "version")
	if composeV2Check.Run() == nil {
		fullArgs := append([]string{"compose"}, args...)
		return exec.CommandContext(ctx, "docker", fullArgs...), "docker compose"
	}
	if path, err := exec.LookPath("docker-compose"); err == nil {
		return exec.CommandContext(ctx, path, args...), "docker-compose"
	}
	fullArgs := append([]string{"compose"}, args...)
	return exec.CommandContext(ctx, "docker", fullArgs...), "docker compose"
}

func (d *DockerManager) CreateNetwork(ctx context.Context, name string) ([]string, error) {
	logLines := []string{fmt.Sprintf("Creating docker network %s...", name)}
	cmd := exec.CommandContext(ctx, "docker", "network", "create", name)
	output, err := cmd.CombinedOutput()
	outputText := strings.TrimSpace(string(output))
	if outputText != "" {
		logLines = append(logLines, outputText)
	}
	if err != nil {
		if outputText != "" {
			return logLines, fmt.Errorf("cannot create network: %s", outputText)
		}
		return logLines, fmt.Errorf("cannot create network: %w", err)
	}
	logLines = append(logLines, "Network created.")
	return logLines, nil
}

func (d *DockerManager) DeleteNetwork(ctx context.Context, name string) ([]string, error) {
	logLines := []string{fmt.Sprintf("Removing docker network %s...", name)}
	cmd := exec.CommandContext(ctx, "docker", "network", "rm", name)
	output, err := cmd.CombinedOutput()
	outputText := strings.TrimSpace(string(output))
	if outputText != "" {
		logLines = append(logLines, outputText)
	}
	if err != nil {
		if outputText != "" {
			return logLines, fmt.Errorf("cannot remove network: %s", outputText)
		}
		return logLines, fmt.Errorf("cannot remove network: %w", err)
	}
	logLines = append(logLines, "Network removed.")
	return logLines, nil
}

func (d *DockerManager) GetLogsPreview(
	ctx context.Context,
	containerID string,
	lineLimit int,
) ([]string, error) {
	if strings.TrimSpace(containerID) == "" {
		return []string{}, nil
	}
	if lineLimit <= 0 {
		lineLimit = 20
	}
	cmd := exec.CommandContext(
		ctx,
		"docker",
		"logs",
		"--tail",
		fmt.Sprintf("%d", lineLimit),
		containerID,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return []string{}, err
	}
	raw := strings.TrimSpace(string(output))
	if raw == "" {
		return []string{}, nil
	}
	return strings.Split(raw, "\n"), nil
}

func normalizeContainerStatus(raw string) string {
	status := strings.ToLower(strings.TrimSpace(raw))
	if strings.HasPrefix(status, "up") {
		return "running"
	}
	return "stopped"
}

func splitCSV(raw string) []string {
	clean := strings.TrimSpace(raw)
	if clean == "" {
		return []string{}
	}
	parts := strings.Split(clean, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value != "" {
			values = append(values, value)
		}
	}
	return values
}
