package main
import (
	"fmt"
	"github.com/shirou/gopsutil/v4/host"
)
func main() {
	platform, family, version, err := host.PlatformInformation()
	fmt.Printf("Platform: %v\nFamily: %v\nVersion: %v\nErr: %v\n", platform, family, version, err)
    info, _ := host.Info()
    fmt.Printf("Info.Platform: %v\nInfo.PlatformVersion: %v\nInfo.PlatformFamily: %v\n", info.Platform, info.PlatformVersion, info.PlatformFamily)
}
