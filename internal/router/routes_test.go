package router

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tug.sh/services/agent/internal/config"
	"tug.sh/services/agent/internal/docker"
)

func TestUpsertRouteAppendsNewDomain(t *testing.T) {
	routes, err := upsertRoute(nil, Route{Domain: "app.example.com", Target: "web", Port: 3000})
	if err != nil {
		t.Fatalf("upsertRoute failed: %v", err)
	}
	if len(routes) != 1 || routes[0].Domain != "app.example.com" {
		t.Fatalf("unexpected routes %+v", routes)
	}
}

func TestUpsertRouteReplacesSameContainer(t *testing.T) {
	existing := []Route{{Domain: "app.example.com", Target: "web", Port: 3000, TargetContainerID: "c1"}}

	byName, err := upsertRoute(existing, Route{Domain: "APP.example.com", Target: "web", Port: 8080})
	if err != nil {
		t.Fatalf("expected a same-container update by name: %v", err)
	}
	if len(byName) != 1 || byName[0].Port != 8080 {
		t.Fatalf("expected the port to be updated in place, got %+v", byName)
	}

	renamed := []Route{{Domain: "app.example.com", Target: "web-old", Port: 3000, TargetContainerID: "c1"}}
	byID, err := upsertRoute(renamed, Route{Domain: "app.example.com", Target: "web-new", Port: 3000, TargetContainerID: "c1"})
	if err != nil {
		t.Fatalf("expected a same-container update by id: %v", err)
	}
	if byID[0].Target != "web-new" {
		t.Fatalf("expected the renamed target to win, got %+v", byID[0])
	}
}

// A domain already pointing at another container must not be silently stolen.
func TestUpsertRouteRejectsForeignContainer(t *testing.T) {
	existing := []Route{{Domain: "app.example.com", Target: "web", Port: 3000, TargetContainerID: "c1"}}
	routes, err := upsertRoute(existing, Route{Domain: "app.example.com", Target: "api", Port: 4000, TargetContainerID: "c2"})
	if err == nil {
		t.Fatal("expected a conflict error")
	}
	if !strings.Contains(err.Error(), "already assigned") {
		t.Fatalf("unexpected error: %v", err)
	}
	if routes[0].Target != "web" {
		t.Fatalf("the existing route must stay untouched, got %+v", routes[0])
	}
}

func TestRenderConfig(t *testing.T) {
	router := New(docker.NewManager(), Spec{HTTPPort: 8080})

	placeholder := router.renderConfig(nil)
	if !strings.Contains(placeholder, ":8080 {") || !strings.Contains(placeholder, "tug-router is running") {
		t.Fatalf("unexpected placeholder config:\n%s", placeholder)
	}

	rendered := router.renderConfig([]Route{
		{Domain: " app.example.com ", Target: " web ", Port: 3000},
		{Domain: "api.example.com", Target: "api", Port: 4000},
	})
	for _, want := range []string{"app.example.com {", "reverse_proxy web:3000", "api.example.com {", "reverse_proxy api:4000"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("missing %q in:\n%s", want, rendered)
		}
	}
}

func TestRoutesRoundTripThroughDisk(t *testing.T) {
	t.Setenv("TUG_DATA_DIR", t.TempDir())
	router := New(docker.NewManager(), Spec{HTTPPort: 80})

	want := []Route{{Domain: "app.example.com", Target: "web", Port: 3000, TargetContainerID: "c1"}}
	if err := router.persistRoutes(want); err != nil {
		t.Fatalf("persistRoutes failed: %v", err)
	}
	got, err := router.loadRoutes("tug-router-abcd")
	if err != nil {
		t.Fatalf("loadRoutes failed: %v", err)
	}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("loadRoutes() = %+v, want %+v", got, want)
	}
}

func TestLoadRoutesWithoutStateIsEmpty(t *testing.T) {
	t.Setenv("TUG_DATA_DIR", t.TempDir())
	router := New(docker.NewManager(), Spec{})

	routes, err := router.loadRoutes("tug-router-none")
	if err != nil {
		t.Fatalf("loadRoutes failed: %v", err)
	}
	if len(routes) != 0 {
		t.Fatalf("expected no routes, got %+v", routes)
	}
}

// Older agents kept routes in /tmp per container; that state must be adopted
// instead of dropped on upgrade.
func TestLoadRoutesMigratesLegacyFile(t *testing.T) {
	t.Setenv("TUG_DATA_DIR", t.TempDir())
	router := New(docker.NewManager(), Spec{})
	containerName := "tug-router-legacy"

	legacyPath := filepath.Join(os.TempDir(), containerName+"-routes.json")
	legacy := `[{"domain":"legacy.example.com","target":"web","port":3000}]`
	if err := os.WriteFile(legacyPath, []byte(legacy), 0o600); err != nil {
		t.Fatalf("cannot seed the legacy file: %v", err)
	}
	defer os.Remove(legacyPath)

	routes, err := router.loadRoutes(containerName)
	if err != nil {
		t.Fatalf("loadRoutes failed: %v", err)
	}
	if len(routes) != 1 || routes[0].Domain != "legacy.example.com" {
		t.Fatalf("unexpected routes %+v", routes)
	}
	migrated, err := router.loadRoutes("other-container")
	if err != nil || len(migrated) != 1 {
		t.Fatalf("expected the legacy routes to be persisted, got %+v (%v)", migrated, err)
	}
}

func TestDecodeRoutes(t *testing.T) {
	if routes, err := decodeRoutes(nil); err != nil || len(routes) != 0 {
		t.Fatalf("empty input should decode to no routes, got %+v (%v)", routes, err)
	}
	if _, err := decodeRoutes([]byte("not json")); err == nil {
		t.Fatal("expected a decode error for malformed state")
	}
}

func TestSpecFromConfig(t *testing.T) {
	spec := SpecFromConfig(config.Config{
		RouterImage:      " caddy:2 ",
		RouterNetwork:    " tug-net ",
		RouterHTTPPort:   8080,
		RouterHTTPSPort:  8443,
		RouterConfigPath: " /etc/caddy/Caddyfile ",
	})
	if spec.Image != "caddy:2" || spec.Network != "tug-net" {
		t.Fatalf("expected trimmed values, got %+v", spec)
	}
	if spec.HTTPPort != 8080 || spec.HTTPSPort != 8443 {
		t.Fatalf("unexpected ports in %+v", spec)
	}
	if spec.RestartPolicy != restartPolicy {
		t.Fatalf("RestartPolicy = %q, want %q", spec.RestartPolicy, restartPolicy)
	}
	wantReload := []string{"caddy", "reload", "--config", "/etc/caddy/Caddyfile"}
	if strings.Join(spec.ReloadCommand, " ") != strings.Join(wantReload, " ") {
		t.Fatalf("ReloadCommand = %v, want %v", spec.ReloadCommand, wantReload)
	}
}

func TestGenerateContainerName(t *testing.T) {
	first, err := generateContainerName()
	if err != nil {
		t.Fatalf("generateContainerName failed: %v", err)
	}
	if !strings.HasPrefix(first, containerPrefix) {
		t.Fatalf("name %q should start with %q", first, containerPrefix)
	}
	second, _ := generateContainerName()
	if first == second {
		t.Fatal("expected unique container names")
	}
}
