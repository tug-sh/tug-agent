package agent

import (
	"fmt"
	"os"
	"path/filepath"
)

type FileManager struct{}

func NewFileManager() *FileManager {
	return &FileManager{}
}

func (f *FileManager) List(relativePath string) ([]os.DirEntry, error) {
	path, err := ResolveSandboxPath(relativePath)
	if err != nil {
		return nil, err
	}
	return os.ReadDir(path)
}

func (f *FileManager) Read(relativePath string) ([]byte, error) {
	path, err := ResolveSandboxPath(relativePath)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func (f *FileManager) Write(relativePath string, data []byte, mode os.FileMode) error {
	path, err := ResolveSandboxPath(relativePath)
	if err != nil {
		return err
	}
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o750); mkErr != nil {
		return fmt.Errorf("cannot create parent directory: %w", mkErr)
	}
	if writeErr := os.WriteFile(path, data, mode); writeErr != nil {
		return fmt.Errorf("cannot write sandbox file: %w", writeErr)
	}
	return nil
}

func (f *FileManager) Delete(relativePath string) error {
	path, err := ResolveSandboxPath(relativePath)
	if err != nil {
		return err
	}
	return os.RemoveAll(path)
}
