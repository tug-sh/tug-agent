module tug.sh/services/agent

go 1.25.0

require (
	github.com/creack/pty v1.1.24
	github.com/gorilla/websocket v1.5.3
	github.com/shirou/gopsutil/v4 v4.26.6
	gopkg.in/natefinch/lumberjack.v2 v2.2.1
	tug.sh/pkg/protocol v0.0.0
)

// The protocol lives in this repository and is compiled by both sides, which is
// what stops the agent and the API from drifting apart.
replace tug.sh/pkg/protocol => ../../pkg/protocol

require (
	github.com/ebitengine/purego v0.10.0 // indirect
	github.com/go-ole/go-ole v1.2.6 // indirect
	github.com/lufia/plan9stats v0.0.0-20211012122336-39d0f177ccd0 // indirect
	github.com/power-devops/perfstat v0.0.0-20240221224432-82ca36839d55 // indirect
	github.com/tklauser/go-sysconf v0.3.16 // indirect
	github.com/tklauser/numcpus v0.11.0 // indirect
	github.com/yusufpapurcu/wmi v1.2.4 // indirect
	golang.org/x/sys v0.47.0 // indirect
)
