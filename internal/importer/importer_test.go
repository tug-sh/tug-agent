package importer

import (
	"strings"
	"testing"
)

func TestExtractRoutesAndCleanCompose(t *testing.T) {
	rawCompose := `version: '3.8'
services:
  web:
    image: nginx:alpine
    ports:
      - "80:80"
    labels:
      - "traefik.enable=true"
      - "traefik.http.routers.web.rule=Host('app.example.com')"
      - "traefik.http.routers.web.entrypoints=websecure"
      - "traefik.http.services.web.loadbalancer.server.port=80"
      - "coolify.managed=true"
  db:
    image: postgres:15
    environment:
      POSTGRES_PASSWORD: secret
`

	result := ExtractRoutesAndCleanCompose(rawCompose)

	if len(result.Routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(result.Routes))
	}

	route := result.Routes[0]
	if route.Domain != "app.example.com" {
		t.Errorf("expected domain app.example.com, got %s", route.Domain)
	}
	if route.TargetPort != 80 {
		t.Errorf("expected target port 80, got %d", route.TargetPort)
	}
	if route.TargetService != "web" {
		t.Errorf("expected target service 'web', got %s", route.TargetService)
	}

	// Cleaned YAML must not contain Traefik or Coolify labels
	if strings.Contains(result.CleanedYAML, "traefik.") {
		t.Errorf("cleaned YAML still contains traefik labels:\n%s", result.CleanedYAML)
	}
	if strings.Contains(result.CleanedYAML, "coolify.") {
		t.Errorf("cleaned YAML still contains coolify labels:\n%s", result.CleanedYAML)
	}
	if !strings.Contains(result.CleanedYAML, "image: nginx:alpine") {
		t.Errorf("cleaned YAML lost service image:\n%s", result.CleanedYAML)
	}
	if !strings.Contains(result.CleanedYAML, "image: postgres:15") {
		t.Errorf("cleaned YAML lost db service:\n%s", result.CleanedYAML)
	}
}

func TestParseEnvFile(t *testing.T) {
	rawEnv := `
# Configuration
PORT=3000
DATABASE_URL="postgres://user:pass@localhost:5432/db"
NODE_ENV='production'
EMPTY_VAL=
# Comment line
`
	env := ParseEnvFile(rawEnv)

	if env["PORT"] != "3000" {
		t.Errorf("expected PORT=3000, got %q", env["PORT"])
	}
	if env["DATABASE_URL"] != "postgres://user:pass@localhost:5432/db" {
		t.Errorf("expected unquoted DATABASE_URL, got %q", env["DATABASE_URL"])
	}
	if env["NODE_ENV"] != "production" {
		t.Errorf("expected unquoted NODE_ENV, got %q", env["NODE_ENV"])
	}
	if _, ok := env["EMPTY_VAL"]; !ok {
		t.Errorf("expected EMPTY_VAL to be present in map")
	}
	if _, ok := env["# Comment line"]; ok {
		t.Errorf("comments should not be in map")
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"My Great App":      "my-great-app",
		"coolify_service_1": "coolify-service-1",
		"Dokploy-App!":      "dokploy-app",
		"  spaces  ":        "spaces",
	}

	for in, expected := range cases {
		got := slugify(in)
		if got != expected {
			t.Errorf("slugify(%q) = %q, want %q", in, got, expected)
		}
	}
}

func TestExtractVersionFromImage(t *testing.T) {
	if got := extractVersionFromImage("dokploy/dokploy:v0.8.4"); got != "v0.8.4" {
		t.Errorf("extractVersionFromImage() = %q, want v0.8.4", got)
	}
	if got := extractVersionFromImage("ghcr.io/coollabsio/coolify:v4.0.0-beta.330"); got != "v4.0.0-beta.330" {
		t.Errorf("extractVersionFromImage() = %q, want v4.0.0-beta.330", got)
	}
}
