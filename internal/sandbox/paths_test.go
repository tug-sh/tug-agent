package sandbox

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestResolvePathRejectsTraversal(t *testing.T) {
	escapes := []string{
		"../etc/passwd",
		"projects/../../etc/passwd",
		"..",
		"a/b/../../../c",
	}
	for _, input := range escapes {
		if resolved, err := ResolvePath(input); err == nil {
			t.Errorf("expected sandbox violation for %q, got %q", input, resolved)
		}
	}
}

func TestResolvePathStaysInsideRoot(t *testing.T) {
	sandboxRoot, err := ResolvePath(".")
	if err != nil {
		t.Fatalf("cannot resolve sandbox root: %v", err)
	}

	cases := map[string]string{
		"projects/app":  filepath.Join(sandboxRoot, "projects", "app"),
		"/projects/app": filepath.Join(sandboxRoot, "projects", "app"),
		"./projects/":   filepath.Join(sandboxRoot, "projects"),
		"/":             sandboxRoot,
		"":              sandboxRoot,
		// An absolute-style path that climbs above "/" is normalized by the
		// filesystem rules first, so it lands inside the sandbox rather than
		// escaping it.
		"/../etc/passwd": filepath.Join(sandboxRoot, "etc", "passwd"),
	}
	for input, want := range cases {
		got, err := ResolvePath(input)
		if err != nil {
			t.Errorf("ResolvePath(%q) failed: %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("ResolvePath(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestDataDirPrefersEnvironment(t *testing.T) {
	custom := t.TempDir()
	t.Setenv("TUG_DATA_DIR", custom)
	if got := DataDir(); got != custom {
		t.Fatalf("DataDir() = %q, want %q", got, custom)
	}

	t.Setenv("TUG_DATA_DIR", "")
	t.Setenv("ENV", "production")
	if got := DataDir(); got != "/var/lib/tug" {
		t.Fatalf("DataDir() = %q, want the production default", got)
	}
}

func TestFileManagerRejectsTraversal(t *testing.T) {
	files := NewFileManager()

	if _, err := files.List("../.."); err == nil {
		t.Error("expected List to reject a traversal path")
	}
	if _, err := files.Read("../../etc/passwd"); err == nil {
		t.Error("expected Read to reject a traversal path")
	}
	if err := files.Write("../../tmp/evil", []byte("x"), 0o600); err == nil {
		t.Error("expected Write to reject a traversal path")
	}
	err := files.Delete("../../etc")
	if err == nil {
		t.Fatal("expected Delete to reject a traversal path")
	}
	if !strings.Contains(err.Error(), "sandbox violation") {
		t.Fatalf("expected a sandbox violation error, got %v", err)
	}
}
