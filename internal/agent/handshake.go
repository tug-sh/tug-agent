package agent

type HandshakeContainer struct {
	ID          string   `json:"id"`
	ProjectID   string   `json:"project_id"`
	Name        string   `json:"name"`
	Image       string   `json:"image"`
	Ports       string   `json:"ports"`
	Status      string   `json:"status"`
	Networks    []string `json:"networks"`
	LogsPreview []string `json:"logs_preview"`
}

type Handshake struct {
	Type          string               `json:"type"`
	ServerID      string               `json:"server_id"`
	WorkspaceID   string               `json:"workspace_id"`
	HostName      string               `json:"host_name"`
	AgentVersion  string               `json:"agent_version"`
	OS            string               `json:"os"`
	Arch          string               `json:"arch"`
	CPUCores      int                  `json:"cpu_cores"`
	RAMBytes      uint64               `json:"ram_bytes"`
	DiskFreeBytes uint64               `json:"disk_free_bytes"`
	LocalIP       string               `json:"local_ip"`
	PublicIP      string               `json:"public_ip"`
	DockerVersion string               `json:"docker_version"`
	Networks      []string             `json:"networks"`
	Containers    []HandshakeContainer `json:"containers"`
}
