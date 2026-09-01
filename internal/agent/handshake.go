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
	"github.com/shirou/gopsutil/v4/host"

	"tug.sh/pkg/protocol"
	"tug.sh/services/agent/internal/sandbox"
	"tug.sh/services/agent/internal/security"
	"tug.sh/services/agent/internal/system"
)

const (
	handshakeSnapshotTimeout = 15 * time.Second
	publicIPTimeout          = 5 * time.Second
	// Above this many pending events a fresh snapshot would only add to the
	// backlog, so it is skipped until the queue drains.
	snapshotBacklogLimit = 24
)

// sendSnapshot reports the whole machine: host facts, resources and every
// container. It is a fact, so it goes through the durable queue and the API
// acknowledges it; the flush loop puts it on the wire.
//
// The old agent wrote the handshake directly to the socket *and* queued a copy,
// which is why a reconnecting agent could apply two snapshots a few seconds
// apart and undo state in between.
func (runtime *Runtime) sendSnapshot() error {
	hello, err := runtime.buildSnapshot()
	if err != nil {
		return err
	}
	if runtime.snapshotWouldPileUp() {
		return nil
	}
	if err := runtime.emitFact(protocol.EntityRuntime, protocol.ActionSnapshot, "", hello); err != nil {
		return err
	}
	runtime.log.Debug("snapshot queued: containers=%d", len(hello.Containers))
	return nil
}

// snapshotWouldPileUp declines to add another full snapshot when one is already
// waiting or the queue is deep. A snapshot is the largest message the agent
// sends and repeating it only slows the drain.
func (runtime *Runtime) snapshotWouldPileUp() bool {
	pending := runtime.eventQueue.PendingCount()
	if pending <= snapshotBacklogLimit && !runtime.eventQueue.HasPendingSnapshot() {
		return false
	}
	runtime.log.Debug("snapshot skipped: pending=%d", pending)
	return true
}

func (runtime *Runtime) buildSnapshot() (protocol.Handshake, error) {
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
		HostName:       hostName,
		AgentVersion:   runtime.config.AgentVersion,
		OS:             getOSDisplayString(),
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
	}, nil
}

func (runtime *Runtime) sendHeartbeat(conn *websocket.Conn) error {
	cpuPct, _ := system.CPUUsagePercent()
	ramUsed, ramTotal, _, _ := system.RAMUsage()
	diskFree, diskTotal, _ := system.DiskStats("/")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	containerStats, _ := runtime.dockerManager.CollectContainerStats(ctx)

	heartbeat := protocol.Heartbeat{
		AgentVersion:     strings.TrimSpace(runtime.config.AgentVersion),
		SentAtUnix:       time.Now().Unix(),
		CPUPercent:       cpuPct,
		RAMUsedBytes:     ramUsed,
		RAMTotalBytes:    ramTotal,
		DiskFreeBytes:    diskFree,
		DiskTotalBytes:   diskTotal,
		ContainerMetrics: containerStats,
		SecurityStatus:   security.InspectSecurityStatus(ctx),
	}
	if err := runtime.emitSignal(conn, protocol.EntityRuntime, protocol.ActionHeartbeat, heartbeat); err != nil {
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

func getOSDisplayString() string {
	if goruntime.GOOS == "linux" {
		if content, err := os.ReadFile("/etc/os-release"); err == nil {
			for _, line := range strings.Split(string(content), "\n") {
				if strings.HasPrefix(line, "PRETTY_NAME=") {
					val := strings.TrimPrefix(line, "PRETTY_NAME=")
					val = strings.Trim(val, `"'`)
					if val != "" {
						return val
					}
				}
			}
		}
	}

	info, err := host.Info()
	if err == nil && info.Platform != "" {
		version := info.PlatformVersion
		platform := info.Platform
		if len(platform) > 0 {
			platform = strings.ToUpper(platform[:1]) + platform[1:]
		}
		if version != "" {
			return platform + " " + version
		}
		return platform
	}
	return goruntime.GOOS
}
