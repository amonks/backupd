package daemon

import "testing"

// The failures below are the daemon's own error text as the remote
// produced it — most of them verbatim from a month of thor's records,
// FreeBSD's "Fssh_" symbol prefix and ssh's stray carriage returns
// included — because the classifier reads exactly this text and nothing
// cleaner.
func TestClassifyFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		msg      string
		kind     string
		expected bool
	}{
		{
			name:     "identification read and banner exchange timed out",
			msg:      "getting remote snapshots for '/tank/movies': zfs list: running 'ssh': exit status 255: Fssh_kex_exchange_identification: read: Operation timed out\r; banner exchange: Connection to 192.0.2.10 port 22: Operation timed out",
			kind:     "timeout",
			expected: true,
		},
		{
			name:     "tcp connect timed out",
			msg:      "handling incomplete transfer of '/tank/cache': getting resume token for '/tank/cache': zfs list: running 'ssh': exit status 255: ssh: connect to host backup.example.net port 22: Operation timed out\n",
			kind:     "timeout",
			expected: true,
		},
		{
			name:     "tcp connect timed out, in Linux's words",
			msg:      "getting remote datasets: zfs list: running 'ssh': exit status 255: ssh: connect to host backup.example.net port 22: Connection timed out",
			kind:     "timeout",
			expected: true,
		},
		{
			name:     "an established session timed out",
			msg:      "getting remote snapshots for '/osm': zfs list: running 'ssh': exit status 255: Connection to 192.0.2.10 port 22 timed out",
			kind:     "timeout",
			expected: true,
		},
		{
			name:     "the remote closed the connection before the session",
			msg:      "getting remote snapshots for '/seeds': zfs list: running 'ssh': exit status 255: Connection closed by 192.0.2.10 port 22",
			kind:     "connection-reset",
			expected: true,
		},
		{
			name:     "the remote closed the connection during identification",
			msg:      "getting remote datasets: zfs list: running 'ssh': exit status 255: kex_exchange_identification: Connection closed by remote host",
			kind:     "connection-reset",
			expected: true,
		},
		{
			name:     "the remote reset the connection",
			msg:      "getting remote datasets: zfs list: running 'ssh': exit status 255: kex_exchange_identification: read: Connection reset by peer\r; Connection reset by 192.0.2.10 port 22",
			kind:     "connection-reset",
			expected: true,
		},
		{
			name:     "a transfer lost the remote mid-stream",
			msg:      "applying op 'transfer' to zfs env (attempt 1) of '/music/flac': process error: 'to' command error: exit status 255 (client_loop: send disconnect: Broken pipe\r\n)",
			kind:     "connection-reset",
			expected: true,
		},
		{
			name:     "a transfer's session was closed by the remote",
			msg:      "applying op 'transfer' to zfs env (attempt 1) of '/music/flac': process error: 'to' command error: exit status 255 (Connection to backup.example.net closed by remote host.\r\n)",
			kind:     "connection-reset",
			expected: true,
		},
		{
			name:     "nothing listening",
			msg:      "getting remote datasets: zfs list: running 'ssh': exit status 255: ssh: connect to host backup.example.net port 22: Connection refused",
			kind:     "connection-refused",
			expected: true,
		},
		{
			name:     "no route",
			msg:      "getting remote datasets: zfs list: running 'ssh': exit status 255: ssh: connect to host backup.example.net port 22: No route to host",
			kind:     "network",
			expected: true,
		},
		{
			name:     "no network",
			msg:      "getting remote datasets: zfs list: running 'ssh': exit status 255: ssh: connect to host backup.example.net port 22: Network is unreachable",
			kind:     "network",
			expected: true,
		},

		// Faults: ssh reached a host and something about it, or about us,
		// is wrong. Retrying will not fix any of these.
		{
			name: "the remote's host key changed",
			msg:  "getting remote snapshots for '/osm': zfs list: running 'ssh': exit status 255: @@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@; @    WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!     @; @@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@; IT IS POSSIBLE THAT SOMEONE IS DOING SOMETHING NASTY!; Someone could be eavesdropping on you right now (man-in-the-middle attack)!; It is also possible that a host key has just been changed.; The fingerprint for the ED25519 key sent by the remote host is; SHA256:Bq6uVTwqvXqjVpfrn3ZwD3BZ3iRJpRSmT1qyGC8mVxE.; Please contact your system administrator.; Add correct host key in /home/backup/.ssh/known_hosts to get rid of this message.; Offending ED25519 key in /home/backup/.ssh/known_hosts:29; Host key for backup.example.net has changed and you have requested strict checking.; Host key verification failed.\n",
			kind: "host-key-mismatch",
		},
		{
			// A refused host key behind a proxy command is followed by
			// the proxy's hang-up, which in isolation is weather: the
			// refusal has to win.
			name: "a refused host key, then the hang-up it causes",
			msg:  "getting remote datasets: zfs list: running 'ssh': exit status 255: Host key verification failed.\r; Connection closed by UNKNOWN port 65535",
			kind: "host-key-mismatch",
		},
		{
			name: "the remote refused our key",
			msg:  "getting remote datasets: zfs list: running 'ssh': exit status 255: ajm@backup.example.net: Permission denied (publickey).",
			kind: "permission",
		},
		{
			name: "a local firewall refused the connection",
			msg:  "getting remote datasets: zfs list: running 'ssh': exit status 255: ssh: connect to host backup.example.net port 22: Permission denied",
			kind: "permission",
		},
		{
			name: "the remote's name does not resolve",
			msg:  "getting remote datasets: zfs list: running 'ssh': exit status 255: ssh: Could not resolve hostname backup.example.net: Name does not resolve",
		},
		{
			name: "the remote disconnected us after authentication failures",
			msg:  "getting remote datasets: zfs list: running 'ssh': exit status 255: Received disconnect from 192.0.2.10 port 22:2: Too many authentication failures; Disconnected from 192.0.2.10 port 22",
		},

		// Not ssh's own failure: the phrases mean nothing unless ssh
		// exited 255, which is how it says the failure was its own and
		// not the remote command's.
		{
			name: "the remote zfs failed",
			msg:  "applying op 'transfer' to zfs env (attempt 1) of '/bad': process error: 'to' command error: exit status 1 (cannot receive incremental stream: out of space)",
		},
		{
			name: "the local half of a transfer failed",
			msg:  "applying op 'transfer' to zfs env (attempt 1) of '/bad': process error: 'from' command error: exit status 1 (connection reset by peer)",
		},
		{
			name: "a local pool failed",
			msg:  "getting local datasets: zfs list: pool suspended",
		},
		{
			name: "a receive stream was corrupt",
			msg:  "invalid backup stream (checksum mismatch)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, expected := classifyFailure(tc.msg)
			if kind != tc.kind || expected != tc.expected {
				t.Errorf("classifyFailure = (%q, %v), want (%q, %v)", kind, expected, tc.kind, tc.expected)
			}
		})
	}
}
