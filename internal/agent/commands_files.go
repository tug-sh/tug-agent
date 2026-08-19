package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"tug.sh/pkg/protocol"
)

const (
	managedFileMode      = 0o640
	schedulesStoragePath = "schedules/schedules.json"
)

func (runtime *Runtime) handleFileList(request commandRequest) ([]string, error) {
	entries, err := runtime.fileManager.List(request.command.FilePath)
	if err != nil {
		return nil, err
	}
	var listing []protocol.DirectoryEntry
	for _, entry := range entries {
		info, _ := entry.Info()
		size := int64(0)
		if info != nil {
			size = info.Size()
		}
		listing = append(listing, protocol.DirectoryEntry{
			Name:  entry.Name(),
			IsDir: entry.IsDir(),
			Size:  size,
		})
	}
	request.setPayload(listing)
	return []string{"Listed " + request.command.FilePath}, nil
}

func (runtime *Runtime) handleFileRead(request commandRequest) ([]string, error) {
	data, err := runtime.fileManager.Read(request.command.FilePath)
	if err != nil {
		return nil, err
	}
	*request.payload = json.RawMessage(fmt.Sprintf("%q", string(data)))
	return []string{"Read " + request.command.FilePath}, nil
}

func (runtime *Runtime) handleFileWrite(request commandRequest) ([]string, error) {
	err := runtime.fileManager.Write(
		request.command.FilePath,
		[]byte(request.command.Content),
		managedFileMode,
	)
	if err != nil {
		return nil, err
	}
	return []string{"Written to " + request.command.FilePath}, nil
}

func (runtime *Runtime) handleFileDelete(request commandRequest) ([]string, error) {
	if err := runtime.fileManager.Delete(request.command.FilePath); err != nil {
		return nil, err
	}
	return []string{"Deleted " + request.command.FilePath}, nil
}

func (runtime *Runtime) handleSaveCompose(request commandRequest) ([]string, error) {
	projectID, err := request.requireProjectID()
	if err != nil {
		return nil, err
	}
	if _, err := require(request.command.Content, "compose content"); err != nil {
		return nil, err
	}
	relativePath := filepath.Join("projects", projectID, "docker-compose.yml")
	if err := runtime.fileManager.Write(relativePath, []byte(request.command.Content), managedFileMode); err != nil {
		return nil, err
	}
	return []string{"Compose file saved."}, nil
}

func (runtime *Runtime) handleCheckHostPath(request commandRequest) ([]string, error) {
	if _, err := require(request.command.FilePath, "file_path"); err != nil {
		return nil, err
	}
	info, err := os.Stat(request.command.FilePath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("path access error: %w", err)
		}
		request.setPayload(protocol.HostPath{Exists: false, IsDir: false})
		return []string{"Path does not exist: " + request.command.FilePath}, nil
	}
	request.setPayload(protocol.HostPath{Exists: true, IsDir: info.IsDir()})
	return []string{"Checked path: " + request.command.FilePath}, nil
}

func (runtime *Runtime) handleCronSchedulesApply(request commandRequest) ([]string, error) {
	normalized := make([]protocol.CronSchedule, 0, len(request.command.Schedules))
	for _, schedule := range request.command.Schedules {
		schedule.ServerID = runtime.config.ServerID
		schedule.Source = "dashboard"
		normalized = append(normalized, schedule)
	}
	raw, marshalErr := json.Marshal(normalized)
	if marshalErr != nil {
		return nil, marshalErr
	}
	if err := runtime.fileManager.Write(schedulesStoragePath, raw, managedFileMode); err != nil {
		return nil, err
	}
	return []string{fmt.Sprintf("Saved %d schedule(s).", len(normalized))}, nil
}

func (runtime *Runtime) handleCronSchedulesPull(request commandRequest) ([]string, error) {
	schedules := make([]protocol.CronSchedule, 0)
	if raw, readErr := runtime.fileManager.Read(schedulesStoragePath); readErr == nil {
		_ = json.Unmarshal(raw, &schedules)
	}
	snapshot := protocol.CronSnapshot{Schedules: schedules}
	if err := runtime.emitFact(protocol.EntityCron, protocol.ActionSnapshot, "", snapshot); err != nil {
		return nil, err
	}
	return []string{fmt.Sprintf("Pulled %d schedule(s).", len(schedules))}, nil
}
