package agent

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"tug.sh/services/agent/internal/protocol"
	"tug.sh/services/agent/internal/sandbox"
	"tug.sh/services/agent/internal/system"
)

const (
	handshakeSnapshotTimeout = 15 * time.Second
	publicIPTimeout          = 5 * time.Second
	// Above this many pending events a fresh snapshot would only add to the
	// backlog, so it is skipped until the queue drains.
	snapshotBacklogLimit = 24
)

// sendHandshake pushes the full host snapshot. enqueueSnapshot additionally
// stores it in the durable event queue, which is what makes a snapshot survive
// a dropped connection.
func (runtime *Runtime) sendHandshake(conn *websocket.Conn, enqueueSnapshot bool) error {
	hello, err := runtime.buildHandshake()
	if err != nil {
		return err
	}

	if err := runtime.writeJSON(conn, hello); err != nil {
		return fmt.Errorf("cannot write handshake: %w", err)
	}
	if runtime.config.ProtocolV2Enabled && enqueueSnapshot {
		runtime.enqueueSnapshotEvent(hello)
	}
	runtime.log.Debug("handshake payload sent: containers=%d", len(hello.Containers))
	return nil
}

func (runtime *Runtime) buildHandshake() (protocol.Handshake, error) {
	publicIP, _ := fetchPublicIP()
	localIP := detectLocalIP()
	dockerVersion, _ := detectDockerVersion()
	ctx, cancel := context.WithTimeout(context.Background(), handshakeSnapshotTimeout)
	defer cancel()
	containers, _ := runtime.dockerManager.ListContainers(ctx)
	networks, _ := runtime.dockerManager.ListNetworks(ctx)
	hostName, _ := os.Hostname()
	ramUsed, totalRAMBytes, _, _ := system.RAMUsage()
	diskFreeBytes, diskTotalBytes, _ := system.DiskStats(sandbox.DataDir())

	runtime.log.Debug(
		"build handshake snapshot: host=%s local_ip=%s public_ip=%s docker=%s containers=%d",
		hostName,
		localIP,
		publicIP,
		dockerVersion,
		len(containers),
	)

	return protocol.Handshake{
		Type:           "handshake",
		ServerID:       runtime.config.ServerID,
		WorkspaceID:    runtime.config.WorkspaceID,
		HostName:       hostName,
		AgentVersion:   runtime.config.AgentVersion,
		OS:             goruntime.GOOS,
		Arch:           goruntime.GOARCH,
		CPUCores:       goruntime.NumCPU(),
		RAMBytes:       totalRAMBytes,
		RAMUsedBytes:   ramUsed,
		DiskFreeBytes:  diskFreeBytes,
		DiskTotalBytes: diskTotalBytes,
		LocalIP:        localIP,
		PublicIP:       publicIP,
		DockerVersion:  dockerVersion,
		Networks:       networks,
		Containers:     containers,
		ProtocolCaps:   []string{"command_inbox", "ws_ping", "event_class"},
	}, nil
}

func (runtime *Runtime) sendHeartbeat(conn *websocket.Conn) error {
	cpuPct, _ := system.CPUUsagePercent()
	ramUsed, ramTotal, _, _ := system.RAMUsage()
	diskFree, diskTotal, _ := system.DiskStats("/")

	heartbeat := protocol.Heartbeat{
		Type:           "heartbeat",
		ServerID:       strings.TrimSpace(runtime.config.ServerID),
		WorkspaceID:    strings.TrimSpace(runtime.config.WorkspaceID),
		AgentVersion:   strings.TrimSpace(runtime.config.AgentVersion),
		SentAtUnix:     time.Now().Unix(),
		CPUPercent:     cpuPct,
		RAMUsedBytes:   ramUsed,
		RAMTotalBytes:  ramTotal,
		DiskFreeBytes:  diskFree,
		DiskTotalBytes: diskTotal,
	}
	if err := runtime.writeJSON(conn, heartbeat); err != nil {
		return fmt.Errorf("cannot write heartbeat: %w", err)
	}
	return nil
}

func detectDockerVersion() (string, error) {
	cmd := exec.Command("docker", "version", "--format", "{{.Server.Version}}")
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func detectLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() || ipNet.IP.To4() == nil {
			continue
		}
		return ipNet.IP.String()
	}
	return ""
}

func fetchPublicIP() (string, error) {
	req, err := http.NewRequest(http.MethodGet, "https://api.ipify.org", nil)
	if err != nil {
		return "", err
	}

	client := &http.Client{Timeout: publicIPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}
