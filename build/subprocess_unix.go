//go:build unix

package build

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// killGroupOnCancel starts cmd in a process group of its own and makes the
// cancellation of its context kill that whole group, so a build tool's
// children (the compilers make or cmake run, the commands of a shell) die
// with it instead of running on, and holding its output pipes open, after
// the build was cancelled.
func killGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// The group's id is the child's pid, as Setpgid makes it.
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}

		return err
	}
}
