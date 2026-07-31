//go:build windows

package agent

import (
	"os/exec"
	"time"
)

// configureCancelableCmd sets WaitDelay so CombinedOutput returns after
// context cancel even when child processes keep I/O pipes open. Windows has
// no Setpgid; CommandContext's default Kill still terminates the process.
func configureCancelableCmd(cmd *exec.Cmd) {
	cmd.WaitDelay = 500 * time.Millisecond
}
