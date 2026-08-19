package router

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"tug.sh/services/agent/internal/logging"
	"tug.sh/services/agent/internal/sandbox"
)

type Route struct {
	Domain            string `json:"domain"`
	Target            string `json:"target"`
	Port              int    `json:"port"`
	TargetContainerID string `json:"target_container_id,omitempty"`
}

// RouteRequest is a dashboard request to publish a container under a domain.
type RouteRequest struct {
	Domain              string
	TargetContainerID   string
	TargetContainerName string
	TargetPort          int
}

func (router *Router) ConfigureRoute(ctx context.Context, request RouteRequest) ([]string, error) {
	desired := Route{
		Domain:            strings.ToLower(strings.TrimSpace(request.Domain)),
		Target:            strings.TrimSpace(request.TargetContainerName),
		Port:              request.TargetPort,
		TargetContainerID: strings.TrimSpace(request.TargetContainerID),
	}
	transcript := logging.NewTranscript(
		"Configuring TugRouter route...",
		fmt.Sprintf("Domain: %s", desired.Domain),
		fmt.Sprintf("Target: %s:%d", desired.Target, desired.Port),
	)

	containerName, resolveErr := router.ResolveContainerName(ctx)
	if resolveErr != nil {
		return transcript.Lines(), ErrNotInstalled
	}
	routes, err := router.loadRoutes(containerName)
	if err != nil {
		return transcript.Lines(), err
	}
	updatedRoutes, upsertErr := upsertRoute(routes, desired)
	if upsertErr != nil {
		return transcript.Lines(), upsertErr
	}

	applyLogs, applyErr := router.applyRoutes(ctx, containerName, updatedRoutes)
	transcript.Merge(applyLogs)
	if applyErr != nil {
		return transcript.Lines(), applyErr
	}
	return transcript.Done("Route applied successfully.")
}

func (router *Router) DeleteRoute(ctx context.Context, domain string) ([]string, error) {
	trimmedDomain := strings.TrimSpace(domain)
	transcript := logging.NewTranscript(
		"Removing TugRouter route...",
		fmt.Sprintf("Domain: %s", trimmedDomain),
	)
	if trimmedDomain == "" {
		return transcript.Fail("domain is required")
	}
	containerName, resolveErr := router.ResolveContainerName(ctx)
	if resolveErr != nil {
		return transcript.Lines(), ErrNotInstalled
	}
	routes, err := router.loadRoutes(containerName)
	if err != nil {
		return transcript.Lines(), err
	}

	remaining := make([]Route, 0, len(routes))
	for _, route := range routes {
		if strings.EqualFold(strings.TrimSpace(route.Domain), trimmedDomain) {
			continue
		}
		remaining = append(remaining, route)
	}
	if len(remaining) == len(routes) {
		return transcript.Fail("route for domain %s not found", trimmedDomain)
	}

	applyLogs, applyErr := router.applyRoutes(ctx, containerName, remaining)
	transcript.Merge(applyLogs)
	if applyErr != nil {
		return transcript.Lines(), applyErr
	}
	return transcript.Done("Route removed successfully.")
}

func (router *Router) ListRoutes(ctx context.Context) ([]Route, error) {
	containerName, resolveErr := router.ResolveContainerName(ctx)
	if resolveErr != nil {
		return nil, ErrNotInstalled
	}
	return router.loadRoutes(containerName)
}

// upsertRoute replaces the route for the desired domain, or appends it when the
// domain is still free. Reassigning a domain to a different container is
// rejected so two projects cannot silently steal each other's traffic.
func upsertRoute(routes []Route, desired Route) ([]Route, error) {
	for index := range routes {
		if !strings.EqualFold(routes[index].Domain, desired.Domain) {
			continue
		}
		existingTargetID := strings.TrimSpace(routes[index].TargetContainerID)
		existingTargetName := strings.TrimSpace(routes[index].Target)
		sameContainerByID := existingTargetID != "" &&
			desired.TargetContainerID != "" &&
			existingTargetID == desired.TargetContainerID
		sameContainerByName := strings.EqualFold(existingTargetName, desired.Target)
		if !sameContainerByID && !sameContainerByName {
			return routes, fmt.Errorf(
				"domain %s is already assigned to container %s",
				desired.Domain,
				existingTargetName,
			)
		}
		routes[index] = desired
		return routes, nil
	}
	return append(routes, desired), nil
}

func (router *Router) applyRoutes(
	ctx context.Context,
	containerName string,
	routes []Route,
) ([]string, error) {
	transcript := logging.NewTranscript()
	if persistErr := router.persistRoutes(routes); persistErr != nil {
		return transcript.Lines(), persistErr
	}

	stagedConfigPath := router.stagedConfigPath(containerName)
	if writeErr := os.WriteFile(stagedConfigPath, []byte(router.renderConfig(routes)), 0o644); writeErr != nil {
		return transcript.Lines(), writeErr
	}

	copyOutput, copyErr := router.docker.CopyToContainer(
		ctx,
		stagedConfigPath,
		containerName,
		router.spec.ConfigPath,
	)
	if copyErr != nil {
		return transcript.Fail("cannot upload router config: %s: %w", copyOutput, copyErr)
	}
	transcript.Addf("Router config updated.")

	reloadOutput, reloadErr := router.docker.ExecInContainer(ctx, containerName, router.spec.ReloadCommand...)
	if reloadErr != nil {
		return transcript.Fail("cannot reload router config: %s: %w", reloadOutput, reloadErr)
	}
	return transcript.Done("Router config reloaded.")
}

// renderConfig builds the Caddyfile served by the router. With no routes
// configured it still answers on the published HTTP port, so an installation
// can be verified before the first domain is attached.
func (router *Router) renderConfig(routes []Route) string {
	var builder strings.Builder
	if len(routes) == 0 {
		builder.WriteString(fmt.Sprintf(":%d {\n", router.spec.HTTPPort))
		builder.WriteString("\trespond \"tug-router is running\" 200\n")
		builder.WriteString("}\n")
		return builder.String()
	}
	for _, route := range routes {
		builder.WriteString(strings.TrimSpace(route.Domain))
		builder.WriteString(" {\n")
		builder.WriteString("\treverse_proxy ")
		builder.WriteString(strings.TrimSpace(route.Target))
		builder.WriteString(fmt.Sprintf(":%d", route.Port))
		builder.WriteString("\n}\n\n")
	}
	return builder.String()
}

func (router *Router) routesPath() string {
	return filepath.Join(sandbox.DataDir(), stateDirName, "routes.json")
}

func (router *Router) stagedConfigPath(containerName string) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("%s-Caddyfile", containerName))
}

// loadRoutes reads the persisted routes, migrating the legacy per-container file
// from /tmp on first use.
func (router *Router) loadRoutes(containerName string) ([]Route, error) {
	raw, err := os.ReadFile(router.routesPath())
	if err == nil {
		return decodeRoutes(raw)
	}
	if !os.IsNotExist(err) {
		return nil, err
	}

	legacyPath := filepath.Join(os.TempDir(), fmt.Sprintf("%s-routes.json", containerName))
	legacyRaw, legacyErr := os.ReadFile(legacyPath)
	if legacyErr != nil {
		if os.IsNotExist(legacyErr) {
			return []Route{}, nil
		}
		return nil, legacyErr
	}
	legacyRoutes, decodeErr := decodeRoutes(legacyRaw)
	if decodeErr != nil {
		return nil, decodeErr
	}
	_ = router.persistRoutes(legacyRoutes)
	return legacyRoutes, nil
}

func decodeRoutes(raw []byte) ([]Route, error) {
	if len(raw) == 0 {
		return []Route{}, nil
	}
	var routes []Route
	if err := json.Unmarshal(raw, &routes); err != nil {
		return nil, err
	}
	return routes, nil
}

func (router *Router) persistRoutes(routes []Route) error {
	routesRaw, marshalErr := json.MarshalIndent(routes, "", "  ")
	if marshalErr != nil {
		return marshalErr
	}
	path := router.routesPath()
	if mkdirErr := os.MkdirAll(filepath.Dir(path), 0o700); mkdirErr != nil {
		return mkdirErr
	}
	return os.WriteFile(path, routesRaw, 0o600)
}
