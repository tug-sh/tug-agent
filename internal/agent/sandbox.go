package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var sandboxRoot string

func init() {
	sandboxRoot = filepath.Join(GetDataDir(), "apps")
}

func GetDataDir() string {
	if env := os.Getenv("TUG_DATA_DIR"); env != "" {
		return env
	}
	if os.Getenv("ENV") == "development" {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, ".tug")
		}
	}
	return "/var/lib/tug"
}

func ResolveSandboxPath(relativePath string) (string, error) {
	cleanInput := filepath.Clean(relativePath)
	if cleanInput == string(filepath.Separator) || cleanInput == "." {
		cleanInput = "."
	} else if strings.HasPrefix(cleanInput, string(filepath.Separator)) {
		// Treat absolute-style UI paths like "/foo/bar" as sandbox-relative.
		cleanInput = strings.TrimPrefix(cleanInput, string(filepath.Separator))
	}
	target := filepath.Join(sandboxRoot, cleanInput)

	rel, err := filepath.Rel(sandboxRoot, target)
	if err != nil {
		return "", fmt.Errorf("sandbox resolution failed: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", errors.New("sandbox violation detected")
	}

	return target, nil
}
