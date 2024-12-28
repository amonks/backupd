package env

import (
	"fmt"
	"strings"
)

var _ Executor = &Remote{}

type Remote struct {
	sshKey  string
	sshHost string
}

func NewRemote(sshKey, sshHost string) *Remote {
	return &Remote{sshKey, sshHost}
}

func (remote *Remote) Exec(cmd ...string) ([]string, error) {
	return Exec("ssh", "-i", remote.sshKey, remote.sshHost, strings.Join(cmd, " "))
}

func (remote *Remote) Execf(s string, args ...any) ([]string, error) {
	return Exec("ssh", "-i", remote.sshKey, remote.sshHost, fmt.Sprintf(s, args...))
}
