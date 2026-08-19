// Package sandbox owns the agent's data directory and confines every file
// operation to the managed apps tree.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var root = filepath.Join(DataDir(), "apps")

// DataDir returns the base directory for agent state, honouring TUG_DATA_DIR
// and falling back to a home directory path in development.
func DataDir() string {
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

// ResolvePath maps a dashboard supplied path onto the sandbox root and rejects
// any attempt to escape it via "..".
func ResolvePath(relativePath string) (string, error) {
	cleanInput := filepath.Clean(relativePath)
	if cleanInput == string(filepath.Separator) || cleanInput == "." {
		cleanInput = "."
	} else if strings.HasPrefix(cleanInput, string(filepath.Separator)) {
		// Treat absolute-style UI paths like "/foo/bar" as sandbox-relative.
		cleanInput = strings.TrimPrefix(cleanInput, string(filepath.Separator))
	}
	target := filepath.Join(root, cleanInput)

	relative, err := filepath.Rel(root, target)
	if err != nil {
		return "", fmt.Errorf("sandbox resolution failed: %w", err)
	}
	if relative == ".." || strings.HasPrefix(relative, "../") {
		return "", errors.New("sandbox violation detected")
	}

	return target, nil
}
