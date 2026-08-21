//go:build unix

package env

import (
	"os/exec"
	"syscall"
)

// isolate puts a command in its own process group before it starts, so
// terminate can reach past a wrapper to the real worker.
//
// It matters because an escalation prefix is not always transparent:
// sudo forks and waits whenever it allocates a pty or logs I/O (use_pty
// is the default on recent versions), so killing the process the daemon
// started can leave `zfs send` running, reparented to init and still
// holding the pipe the daemon waits to drain. A group kill reaches it
// where a kill of the wrapper does not.
//
// Where it does not reach is across a privilege boundary: sudo is
// already root when it forks, so both processes are root, and
// kill(-pgid) from an unprivileged daemon returns EPERM for every
// member. Cancellation is not left hanging on that — the remote half of
// the pipe runs as the daemon's own user and dies, and closing the pipe
// kills the send with EPIPE, which is what actually stops a root-owned
// child. The group kill is the mechanism where the daemon has the
// privilege to use it (running as root, or wrapping something that does
// not gain privilege), and best-effort where it does not.
func isolate(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// terminate kills a started command's whole process group, falling back
// to the process itself if the group is gone or out of reach. Both can
// fail — see isolate on the privileged case — and neither error is
// actionable here: the caller is already cancelling, and what stops a
// child it cannot signal is the pipe closing behind it.
func terminate(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		cmd.Process.Kill()
	}
}
