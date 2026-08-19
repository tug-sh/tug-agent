package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tug.sh/services/agent/internal/logging"
)

func TestReadEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := strings.Join([]string{
		"# comment",
		"",
		"  DATABASE_URL = postgres://localhost/db  ",
		"EMPTY=",
		"=novalue",
		"malformed",
		"TOKEN=abc=def",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("cannot write the env file: %v", err)
	}

	envVars := readEnvFile(path)
	want := map[string]string{
		"DATABASE_URL": "postgres://localhost/db",
		"EMPTY":        "",
		"TOKEN":        "abc=def",
	}
	if len(envVars) != len(want) {
		t.Fatalf("got %d entries %v, want %d", len(envVars), envVars, len(want))
	}
	for key, value := range want {
		if envVars[key] != value {
			t.Errorf("%s = %q, want %q", key, envVars[key], value)
		}
	}
}

func TestReadEnvFileMissingIsEmpty(t *testing.T) {
	if got := readEnvFile(filepath.Join(t.TempDir(), "absent")); len(got) != 0 {
		t.Fatalf("expected no variables, got %v", got)
	}
}

func TestResolveComposeFilePrefersRequested(t *testing.T) {
	deployDir := t.TempDir()
	writeFile(t, filepath.Join(deployDir, "docker-compose.yml"), "services: {}")
	writeFile(t, filepath.Join(deployDir, "compose.yaml"), "services: {}")

	path, name, err := resolveComposeFile(deployDir, "", logging.NewTranscript())
	if err != nil {
		t.Fatalf("resolveComposeFile failed: %v", err)
	}
	if name != "docker-compose.yml" || path != filepath.Join(deployDir, name) {
		t.Fatalf("got %q at %q, want the default compose file", name, path)
	}
}

func TestResolveComposeFileFallsBackToAlternatives(t *testing.T) {
	deployDir := t.TempDir()
	writeFile(t, filepath.Join(deployDir, "compose.yaml"), "services: {}")

	_, name, err := resolveComposeFile(deployDir, "docker-compose.yml", logging.NewTranscript())
	if err != nil {
		t.Fatalf("resolveComposeFile failed: %v", err)
	}
	if name != "compose.yaml" {
		t.Fatalf("name = %q, want the alternative compose file", name)
	}
}

// A Dockerfile-only repository still deploys: the agent generates a minimal
// compose file for it.
func TestResolveComposeFileGeneratesFromDockerfile(t *testing.T) {
	deployDir := t.TempDir()
	writeFile(t, filepath.Join(deployDir, "Dockerfile"), "FROM alpine")

	path, name, err := resolveComposeFile(deployDir, "", logging.NewTranscript())
	if err != nil {
		t.Fatalf("resolveComposeFile failed: %v", err)
	}
	if name != defaultComposeFile {
		t.Fatalf("name = %q, want %q", name, defaultComposeFile)
	}
	generated, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("the generated compose file is missing: %v", readErr)
	}
	if !strings.Contains(string(generated), "dockerfile: Dockerfile") {
		t.Fatalf("unexpected generated compose file:\n%s", generated)
	}
}

func TestResolveComposeFileFailsWithoutAnything(t *testing.T) {
	if _, _, err := resolveComposeFile(t.TempDir(), "", logging.NewTranscript()); err == nil {
		t.Fatal("expected an error when neither compose nor Dockerfile exists")
	}
}

// The dashboard editor always reads projects/<id>/docker-compose.yml, so an
// alternative file name has to be mirrored there.
func TestMirrorStandardComposeFile(t *testing.T) {
	deployDir := t.TempDir()
	source := filepath.Join(deployDir, "compose.yaml")
	writeFile(t, source, "services:\n  app: {}\n")

	mirrorStandardComposeFile(deployDir, source)

	mirrored, err := os.ReadFile(filepath.Join(deployDir, defaultComposeFile))
	if err != nil {
		t.Fatalf("expected the mirrored file: %v", err)
	}
	if string(mirrored) != "services:\n  app: {}\n" {
		t.Fatalf("unexpected mirrored content:\n%s", mirrored)
	}
}

func TestWriteDeploymentLogRequiresProject(t *testing.T) {
	// Without a project id there is no sandbox path to write to, and the call
	// must stay silent rather than fail a finished deployment.
	writeDeploymentLog("  ", "cmd-1", []string{"line"}, nil)
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("cannot write %s: %v", path, err)
	}
}
