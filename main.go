// cmds:
//     $sshcmd zfs receive -A (dest $ds) || return 1
//     $sendcmd | $sshcmd "$remotecmd"
//     $argv --dryrun --verbose --parsable | tail -n1 | awk '{ print $2 }'
//     zfs list -t snapshot -o name -s creation -d1 $ds | tail -1 | cut -d'@' -f2
//     $sshcmd "zfs list -o receive_resume_token -S name -d1 $target | tail -1" 2>&1

package main

import (
	"fmt"
	"os/user"
)

func main() {
	if err := run(); err != nil {
		panic(err)
	}
}

func run() error {
	if whoami, err := user.Current(); err != nil {
		return fmt.Errorf("getting user: %w", err)
	} else if whoami.Username != "root" {
		return fmt.Errorf("must be root, not '%s'", whoami)
	}

	b := NewBackup(
		NewZFS("data", Local),
		NewZFS(
			"data1/thor",
			NewRemote(
				"/home/ajm/.ssh/id_ed25519",
				"root@57269.zfs.rsync.net",
			),
		),
	)

	if err := b.Init(); err != nil {
		return err
	}

	for _, ds := range b.datasets {
		fmt.Println(ds.name)
	}

	return nil
}
