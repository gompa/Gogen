//go:build unix

package agent

import (
	"os/exec"
	"syscall"
	"time"
)

// configureCancelableCmd makes context cancel terminate the whole process
// group. Plain CommandContext only kills the direct child (usually sh), so
// pipelines and `echo; sleep` leave grandchildren holding stdout/stderr and
// CombinedOutput hangs — which keeps the web turn lock held after Cancel.
func configureCancelableCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err != nil {
			_ = cmd.Process.Kill()
		}
		return err
	}
	// Unblock Wait if anything still holds pipes after the group kill.
	cmd.WaitDelay = 500 * time.Millisecond
}
