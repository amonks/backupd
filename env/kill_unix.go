//go:build unix

package env

import (
	"os/exec"
	"syscall"
)

// isolate puts a command in its own process group before it starts, so
// terminate can take down the whole tree.
//
// This is what makes an escalation prefix safe to cancel. sudo does not
// exec its command — it forks and waits — so killing the process the
// daemon started leaves `zfs send` running, reparented to init, still
// holding the pipe the daemon is waiting to drain. Killing the group
// reaches the real worker whether or not anything is wrapping it.
func isolate(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// terminate kills a started command's whole process group, falling back
// to the process itself if the group is gone.
func terminate(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		cmd.Process.Kill()
	}
}
