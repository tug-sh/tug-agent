package agent

import (
	"fmt"
	"strings"
	"time"

	"tug.sh/services/agent/internal/router"
)

const (
	routerInstallTimeout = 45 * time.Second
	routerRouteTimeout   = 45 * time.Second
	minPort              = 1
	maxPort              = 65535
)

// routerRoutesPayload is the command result body listing the router routes.
type routerRoutesPayload struct {
	Routes []router.Route `json:"routes"`
}

func (runtime *Runtime) handleInstallTugRouter(request commandRequest) ([]string, error) {
	installCtx, cancel := request.withTimeout(routerInstallTimeout)
	defer cancel()

	logs, installErr := runtime.router.Install(installCtx, request.command.Image)
	if installErr != nil {
		return logs, installErr
	}
	if handshakeErr := runtime.sendHandshake(request.conn, false); handshakeErr != nil {
		return logs, handshakeErr
	}
	runtime.enqueueAllRunningContainerDeltas(request.ctx)
	return logs, nil
}

func (runtime *Runtime) handleConfigureTugRouterRoute(request commandRequest) ([]string, error) {
	domain, err := request.requireDomain()
	if err != nil {
		return nil, err
	}
	targetName, err := require(request.command.TargetContainerName, "target_container_name")
	if err != nil {
		return nil, err
	}
	if request.command.TargetPort < minPort || request.command.TargetPort > maxPort {
		return nil, fmt.Errorf("target_port must be between %d and %d", minPort, maxPort)
	}

	configureCtx, cancel := request.withTimeout(routerRouteTimeout)
	defer cancel()

	return runtime.router.ConfigureRoute(configureCtx, router.RouteRequest{
		Domain:              domain,
		TargetContainerID:   strings.TrimSpace(request.command.TargetContainerID),
		TargetContainerName: targetName,
		TargetPort:          request.command.TargetPort,
	})
}

func (runtime *Runtime) handleListTugRouterRoutes(request commandRequest) ([]string, error) {
	routes, err := runtime.router.ListRoutes(request.ctx)
	if err != nil {
		return nil, err
	}
	request.setPayload(routerRoutesPayload{Routes: routes})
	return []string{fmt.Sprintf("Loaded %d route(s).", len(routes))}, nil
}

func (runtime *Runtime) handleRemoveTugRouterRoute(request commandRequest) ([]string, error) {
	domain, err := request.requireDomain()
	if err != nil {
		return nil, err
	}
	removeCtx, cancel := request.withTimeout(routerRouteTimeout)
	defer cancel()

	return runtime.router.DeleteRoute(removeCtx, domain)
}
