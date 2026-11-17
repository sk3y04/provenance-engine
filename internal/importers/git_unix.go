//go:build !windows

package importers

import (
	"os"
	"os/exec"
	"syscall"
)

// configureGitCmd puts git in its own process group and kills the whole group
// on cancellation. git spawns remote helpers (for example git-remote-https)
// that inherit the output pipes; killing only the parent leaves them holding
// the pipes open, which would block CombinedOutput/Output indefinitely.
func configureGitCmd(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
}
