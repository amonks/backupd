package env

import (
	"fmt"
	"os/exec"
	"strings"

	"monks.co/backupd/logger"
)

var _ Executor = &Remote{}

type Remote struct {
	sshKey  string
	sshHost string
}

func NewRemote(sshKey, sshHost string) *Remote {
	return &Remote{sshKey, sshHost}
}

func (remote *Remote) Exec(logger *logger.Logger, cmd ...string) ([]string, error) {
	return Exec(logger, "ssh", "-i", remote.sshKey, remote.sshHost, strings.Join(cmd, " "))
}

func (remote *Remote) Execf(logger *logger.Logger, s string, args ...any) ([]string, error) {
	return Exec(logger, "ssh", "-i", remote.sshKey, remote.sshHost, fmt.Sprintf(s, args...))
}

// Command builds — but does not run — the ssh invocation that runs the
// given command on the remote, for the receive half of a transfer pipe.
func (remote *Remote) Command(args ...string) *exec.Cmd {
	return exec.Command("ssh", "-i", remote.sshKey, remote.sshHost, strings.Join(args, " "))
}
