// Package router owns the edge reverse proxy container: how it is installed,
// where its configuration lives and how routes are rendered into it. Every
// installation detail comes from Spec, so the docker layer stays generic.
package router

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"tug.sh/services/agent/internal/config"
	"tug.sh/services/agent/internal/docker"
	"tug.sh/services/agent/internal/logging"
)

const (
	labelKeyApp        = "tug.app"
	labelKeyManaged    = "tug.managed"
	appLabel           = "tug-router"
	containerPrefix    = "tug-router-"
	stateDirName       = "tug-router"
	restartPolicy      = "unless-stopped"
	containerNameBytes = 4
)

// ErrNotInstalled is returned when a route operation is attempted before the
// router container exists.
var ErrNotInstalled = errors.New("tug-router container is not installed")

// Spec holds every installation specific detail of the edge router, so running
// a different image, publishing other ports or pointing at another config path
// is a configuration change instead of a code change.
type Spec struct {
	Image         string
	Network       string
	RestartPolicy string
	HTTPPort      int
	HTTPSPort     int
	ConfigPath    string
	ReloadCommand []string
}

func SpecFromConfig(cfg config.Config) Spec {
	configPath := strings.TrimSpace(cfg.RouterConfigPath)
	return Spec{
		Image:         strings.TrimSpace(cfg.RouterImage),
		Network:       strings.TrimSpace(cfg.RouterNetwork),
		RestartPolicy: restartPolicy,
		HTTPPort:      cfg.RouterHTTPPort,
		HTTPSPort:     cfg.RouterHTTPSPort,
		ConfigPath:    configPath,
		ReloadCommand: []string{"caddy", "reload", "--config", configPath},
	}
}

type Router struct {
	docker *docker.Manager
	spec   Spec
}

func New(dockerManager *docker.Manager, spec Spec) *Router {
	return &Router{docker: dockerManager, spec: spec}
}

// Install replaces any existing router container with a fresh one. imageOverride
// wins over the configured image, which lets the dashboard pin a specific tag.
func (router *Router) Install(ctx context.Context, imageOverride string) ([]string, error) {
	containerName, nameErr := generateContainerName()
	if nameErr != nil {
		return nil, nameErr
	}
	image := strings.TrimSpace(imageOverride)
	if image == "" {
		image = router.spec.Image
	}

	transcript := logging.NewTranscript("Preparing TugRouter installation...")
	if err := router.removePrevious(ctx, transcript); err != nil {
		return transcript.Lines(), err
	}

	transcript.Addf("Starting TugRouter container %s...", containerName)
	transcript.Addf("Using image %s", image)
	output, runErr := router.docker.RunContainer(ctx, docker.ContainerSpec{
		Name:          containerName,
		Image:         image,
		Network:       router.spec.Network,
		RestartPolicy: router.spec.RestartPolicy,
		Labels: map[string]string{
			labelKeyApp:     appLabel,
			labelKeyManaged: "true",
		},
		Ports: []docker.PortMapping{
			{HostPort: router.spec.HTTPPort, ContainerPort: router.spec.HTTPPort},
			{HostPort: router.spec.HTTPSPort, ContainerPort: router.spec.HTTPSPort},
		},
	})
	transcript.AddCommandOutput(output)
	if runErr != nil {
		return transcript.Fail("cannot start tug-router container: %w", runErr)
	}
	return transcript.Done("TugRouter is installed and running.")
}

// removePrevious drops an existing router container so the new one can bind the
// published ports. A missing container is not an error.
func (router *Router) removePrevious(ctx context.Context, transcript *logging.Transcript) error {
	existingName, resolveErr := router.ResolveContainerName(ctx)
	if resolveErr != nil {
		transcript.Addf("No previous TugRouter container found.")
		return nil
	}
	transcript.Addf("Removing previous TugRouter container %s...", existingName)
	output, removeErr := router.docker.RemoveContainer(ctx, existingName)
	transcript.AddCommandOutput(output)
	if removeErr != nil {
		return fmt.Errorf("cannot remove previous tug-router container: %w", removeErr)
	}
	return nil
}

func (router *Router) ResolveContainerName(ctx context.Context) (string, error) {
	return router.docker.FindContainerNameByLabel(ctx, labelKeyApp+"="+appLabel)
}

func generateContainerName() (string, error) {
	suffix := make([]byte, containerNameBytes)
	if _, err := rand.Read(suffix); err != nil {
		return "", err
	}
	return containerPrefix + strings.ToLower(hex.EncodeToString(suffix)), nil
}
