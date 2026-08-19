package protocol

import "encoding/json"

// Command is the union of every command the API can dispatch to the
// agent over the websocket connection.
type Command struct {
	Type                 string         `json:"type"`
	CommandID            string         `json:"command_id"`
	WorkspaceID          string         `json:"workspace_id"`
	ServerID             string         `json:"server_id"`
	BinaryURL            string         `json:"binary_url"`
	Image                string         `json:"image"`
	CleanDockerResources bool           `json:"clean_docker_resources"`
	ContainerID          string         `json:"container_id"`
	TargetContainerName  string         `json:"target_container_name"`
	TargetContainerID    string         `json:"target_container_id"`
	TargetPort           int            `json:"target_port"`
	Action               string         `json:"action"`
	RemoveVolumes        bool           `json:"remove_volumes"`
	RemoveImage          bool           `json:"remove_image"`
	Domain               string         `json:"domain"`
	NetworkName          string         `json:"network_name"`
	Content              string         `json:"content"`
	Summary              string         `json:"summary"`
	ConfigID             string         `json:"config_id"`
	ProjectID            string         `json:"project_id"`
	ComposeContent       string         `json:"compose_content,omitempty"`
	RepoURL              string         `json:"repo_url"`
	Branch               string         `json:"branch"`
	FilePath             string         `json:"file_path"`
	FileType             string         `json:"file_type"`
	Command              string         `json:"command"`
	TerminalID           string         `json:"terminal_id"`
	Rows                 uint16         `json:"rows"`
	Cols                 uint16         `json:"cols"`
	Payload              string         `json:"payload"`
	Tail                 int            `json:"tail"`
	Schedules            []CronSchedule `json:"schedules"`
}

type CommandResult struct {
	Type      string          `json:"type"`
	CommandID string          `json:"command_id"`
	Success   bool            `json:"success"`
	Error     string          `json:"error,omitempty"`
	Logs      []string        `json:"logs,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type CommandProgress struct {
	Type      string          `json:"type"`
	CommandID string          `json:"command_id"`
	Status    string          `json:"status"`
	Error     string          `json:"error,omitempty"`
	Logs      []string        `json:"logs,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	ServerID  string          `json:"server_id,omitempty"`
}

type Heartbeat struct {
	Type           string  `json:"type"`
	ServerID       string  `json:"server_id"`
	WorkspaceID    string  `json:"workspace_id,omitempty"`
	AgentVersion   string  `json:"agent_version,omitempty"`
	SentAtUnix     int64   `json:"sent_at_unix"`
	CPUPercent     float64 `json:"cpu_percent,omitempty"`
	RAMUsedBytes   uint64  `json:"ram_used_bytes,omitempty"`
	RAMTotalBytes  uint64  `json:"ram_total_bytes,omitempty"`
	DiskFreeBytes  uint64  `json:"disk_free_bytes,omitempty"`
	DiskTotalBytes uint64  `json:"disk_total_bytes,omitempty"`
}

type CronSchedule struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	ServerID    string `json:"server_id"`
	Name        string `json:"name"`
	Command     string `json:"command"`
	Description string `json:"description"`
	Preset      string `json:"preset"`
	Expression  string `json:"expression"`
	Timezone    string `json:"timezone"`
	Enabled     bool   `json:"enabled"`
	Source      string `json:"source"`
	UpdatedAt   string `json:"updated_at"`
}

type CronSchedulesSnapshot struct {
	Type      string         `json:"type"`
	Workspace string         `json:"workspace_id"`
	ServerID  string         `json:"server_id"`
	Schedules []CronSchedule `json:"schedules"`
}

type UpdateProgress struct {
	Type            string `json:"type"`
	CommandID       string `json:"command_id"`
	ServerID        string `json:"server_id"`
	WorkspaceID     string `json:"workspace_id"`
	DownloadedBytes uint64 `json:"downloaded_bytes"`
	TotalBytes      uint64 `json:"total_bytes"`
	Percent         int    `json:"percent"`
}

// ServerFrame carries the fields shared by every server frame, used to
// route a message before it is decoded into its concrete type.
type ServerFrame struct {
	Type  string `json:"type"`
	Error string `json:"error"`
}

// Command result payloads returned to the dashboard.

type DirectoryEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

type ExecResult struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
}

type ContainersSnapshot struct {
	Containers []HandshakeContainer `json:"containers"`
}

type HostPath struct {
	Exists bool `json:"exists"`
	IsDir  bool `json:"is_dir"`
}
