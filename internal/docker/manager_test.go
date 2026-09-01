package docker

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeContainerStatus(t *testing.T) {
	cases := map[string]string{
		"Up 3 hours":              "running",
		"up 2 minutes (healthy)":  "running",
		"Exited (0) 5 hours ago":  "stopped",
		"Created":                 "stopped",
		"  Up About an hour     ": "running",
		"":                        "stopped",
	}
	for raw, want := range cases {
		if got := normalizeContainerStatus(raw); got != want {
			t.Errorf("normalizeContainerStatus(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestSplitCSV(t *testing.T) {
	got := splitCSV(" bridge , tug-net ,, host ")
	want := []string{"bridge", "tug-net", "host"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("splitCSV() = %v, want %v", got, want)
	}
	if empty := splitCSV("   "); len(empty) != 0 {
		t.Errorf("expected an empty slice, got %v", empty)
	}
}

// Labels are sorted so a container started twice produces the same docker
// invocation, which keeps diffs and logs stable.
func TestSortedKeys(t *testing.T) {
	got := sortedKeys(map[string]string{"tug.managed": "true", "tug.app": "router", "a": "1"})
	want := []string{"a", "tug.app", "tug.managed"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sortedKeys() = %v, want %v", got, want)
	}
	if len(sortedKeys(nil)) != 0 {
		t.Error("expected no keys for a nil map")
	}
}

func TestPortMappingString(t *testing.T) {
	if got := (PortMapping{HostPort: 8080, ContainerPort: 80}).String(); got != "8080:80" {
		t.Errorf("PortMapping.String() = %q, want %q", got, "8080:80")
	}
}

func TestRunContainerRequiresNameAndImage(t *testing.T) {
	manager := NewManager()
	ctx := context.Background()

	if _, err := manager.RunContainer(ctx, ContainerSpec{Image: "caddy:2"}); err == nil {
		t.Error("expected an error for a missing container name")
	}
	if _, err := manager.RunContainer(ctx, ContainerSpec{Name: "tug-router-1"}); err == nil {
		t.Error("expected an error for a missing image")
	}
}

func TestEnsureNetworkRequiresName(t *testing.T) {
	if err := NewManager().EnsureNetwork(context.Background(), "   "); err == nil {
		t.Error("expected an error for a blank network name")
	}
}

func TestDeployCommandPrefersCustomCommand(t *testing.T) {
	cmd, description := DeployCommand(context.Background(), "  make deploy  ", "-f", "/apps/x/docker-compose.yml", "up", "-d")
	if description != "make deploy" {
		t.Errorf("description = %q, want the trimmed custom command", description)
	}
	if !strings.HasSuffix(cmd.Path, "sh") {
		t.Errorf("expected the custom command to run through sh, got %q", cmd.Path)
	}
	if len(cmd.Args) != 3 || cmd.Args[1] != "-c" || cmd.Args[2] != "make deploy" {
		t.Errorf("unexpected args %v", cmd.Args)
	}
}

func TestDeployCommandDefaultsToCompose(t *testing.T) {
	_, description := DeployCommand(context.Background(), "", "-f", "/apps/x/docker-compose.yml", "up", "-d", "--build")
	if !strings.Contains(description, "-f /apps/x/docker-compose.yml up -d --build") {
		t.Errorf("expected default command with flags, got %q", description)
	}
}

func TestParseByteSize(t *testing.T) {
	cases := []struct {
		input string
		want  uint64
	}{
		{"0", 0},
		{"--", 0},
		{"", 0},
		{"1024B", 1024},
		{"1KB", 1024},
		{"500kB", 512000},
		{"45.2MiB", 47395635},
		{"1.5GB", 1610612736},
		{"2GiB", 2147483648},
	}
	for _, tc := range cases {
		got := parseByteSize(tc.input)
		if got != tc.want {
			t.Errorf("parseByteSize(%q) = %d, want %d", tc.input, got, tc.want)
		}
	}
}

func TestParseTwoSlashValues(t *testing.T) {
	v1, v2 := parseTwoSlashValues("45.2MiB / 2GiB")
	if v1 != 47395635 || v2 != 2147483648 {
		t.Errorf("parseTwoSlashValues() = (%d, %d), want (47395635, 2147483648)", v1, v2)
	}
}
