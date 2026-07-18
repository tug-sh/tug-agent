package agent

import (
	"fmt"
	"os"
	"os/exec"
)

func RunDetachedUninstall(cleanDocker bool) error {
	mode := "keep"
	if cleanDocker {
		mode = "clean"
	}

	scriptPath := "/tmp/tug-uninstall.sh"
	script := fmt.Sprintf(`#!/usr/bin/env sh
set -eu

systemctl stop tug-agent.service || true
systemctl disable tug-agent.service || true
rm -rf /etc/tug /var/lib/tug

if [ "%s" = "clean" ]; then
  docker ps -aq | xargs -r docker rm -f
fi

rm -f /usr/local/bin/tug-agent /usr/local/bin/tug /usr/local/bin/tug.next
rm -f "$0"
`, mode)

	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		return err
	}

	cmd := exec.Command("nohup", scriptPath)
	if err := cmd.Start(); err != nil {
		return err
	}
	return nil
}
