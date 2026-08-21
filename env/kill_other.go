//go:build !unix

package env

import "os/exec"

// isolate is a no-op off unix: process groups are how the unix build
// reaches a wrapped child (see kill_unix.go), and the platforms without
// them have no zfs either.
func isolate(cmd *exec.Cmd) {}

func terminate(cmd *exec.Cmd) {
	if cmd.Process != nil {
		cmd.Process.Kill()
	}
}
