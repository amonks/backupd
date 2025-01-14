package env

import (
	"fmt"
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

func (remote *Remote) Exec(logger logger.Logger, cmd ...string) ([]string, error) {
	return Exec(logger, "ssh", "-i", remote.sshKey, remote.sshHost, strings.Join(cmd, " "))
}

func (remote *Remote) Execf(logger logger.Logger, s string, args ...any) ([]string, error) {
	return Exec(logger, "ssh", "-i", remote.sshKey, remote.sshHost, fmt.Sprintf(s, args...))
}
