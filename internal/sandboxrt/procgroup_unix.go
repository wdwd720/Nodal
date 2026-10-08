//go:build unix

package sandboxrt

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup puts a build in its own process group, and makes the
// timeout kill the whole group: the compiler and linker the go command
// spawned die with it instead of outliving the build.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
