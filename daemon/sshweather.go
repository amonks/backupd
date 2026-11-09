package daemon

import (
	"fmt"
	"log/slog"
	"strings"

	"monks.co/backupd/history"
)

// A failed cycle is either a fault — something someone has to fix — or
// an expected failure: weather, with a base rate outside anyone's
// control, which needs attention only if it keeps happening. The remote
// is a host across the public internet, reached over ssh, and the path
// to it fails now and then: a connection that times out, is refused, or
// is dropped. A failed cycle is retried after a backoff that starts at
// a minute, and a remote that stays unreachable still pages — a failed
// cycle withholds the snitch ping — and raises the dashboard's
// cycle-failing and stale-backup issues. So a cycle whose every failure
// is ssh failing to reach the remote is logged at WARN, and every other
// failed cycle at ERROR.
//
// The text is all there is to go on: ssh reports its failures on
// stderr, and env carries that text in the error. The phrases below are
// ssh's own, as OpenSSH and the C library print them, each meaning one
// thing — a phrase that could mean two things is left out, and what it
// would have matched stays a fault.

// sshExitStatus is how the failure says ssh itself failed. ssh exits
// with the remote command's status, except when it fails on its own
// account, when it exits 255; a remote zfs that fails exits 1, and its
// words are never read as ssh's.
const sshExitStatus = "exit status 255"

// The error.type a classified failure is recorded under. The names are
// the common transport vocabulary, so a log pipeline grouping by
// error.type groups ssh's failures with everyone else's. dns and tls
// come only from a snitch check-in's error chain (transportKind): ssh's
// words for a hostname that does not resolve are left unclassified, a
// fault, since the remote's name is hand-edited config.
const (
	kindTimeout         = "timeout"
	kindDNS             = "dns"
	kindConnRefused     = "connection-refused"
	kindConnReset       = "connection-reset"
	kindNetwork         = "network"
	kindTLS             = "tls"
	kindHostKeyMismatch = "host-key-mismatch"
	kindPermission      = "permission"
)

type sshPhrase struct {
	phrase string
	kind   string
}

// sshFaults are ssh's words for reaching a host and refusing it, or
// being refused by it. They are checked before the weather: a refusal
// can be followed by a hang-up that, alone, would read as weather — a
// host key refused behind a proxy command ends "Connection closed by
// UNKNOWN port 65535".
var sshFaults = []sshPhrase{
	// The remote is not the host known_hosts pins: the key changed, or
	// someone is in the middle. ssh prints the banner and then its
	// verdict; either is enough.
	{"REMOTE HOST IDENTIFICATION HAS CHANGED", kindHostKeyMismatch},
	{"Host key verification failed", kindHostKeyMismatch},
	// The remote refused our key ("Permission denied (publickey)."), or
	// the local system refused the connect (strerror(EACCES)).
	{"Permission denied", kindPermission},
}

// sshWeather are ssh's words for a connection to the remote that did
// not happen or did not last.
var sshWeather = []sshPhrase{
	// A deadline on the path to the remote. Every "timed out" ssh itself
	// prints is a transport deadline: strerror(ETIMEDOUT) — "Operation
	// timed out" on FreeBSD and macOS, "Connection timed out" on Linux —
	// from the TCP connect ("ssh: connect to host H port 22: …"), the
	// identification read ("kex_exchange_identification: read: …"), and
	// the banner exchange ("banner exchange: Connection to H port 22:
	// …"); and an established session's "Connection to H port 22 timed
	// out".
	{"timed out", kindTimeout},
	// Nothing listening at the remote's address.
	{"Connection refused", kindConnRefused},
	// The remote dropped the connection: a reset (strerror(ECONNRESET),
	// "Connection reset by peer", or ssh's own "Connection reset by H
	// port 22"); a close before the session began ("Connection closed by
	// H port 22", "kex_exchange_identification: Connection closed by
	// remote host"), which is how an sshd at its connection limit turns
	// a client away; a close mid-session ("Connection to H closed by
	// remote host."); or a write into a connection already gone
	// ("client_loop: send disconnect: Broken pipe").
	{"Connection reset by", kindConnReset},
	{"Connection closed by", kindConnReset},
	{"closed by remote host", kindConnReset},
	{"send disconnect: Broken pipe", kindConnReset},
	// No path to the remote at all.
	{"No route to host", kindNetwork},
	{"Network is unreachable", kindNetwork},
	{"Host is down", kindNetwork},
}

// Deliberately absent, so they stay faults: "Could not resolve
// hostname" (a name that does not resolve is as likely a typo in the
// config as a resolver's bad minute), "Disconnected from" (the remote
// hanging up on purpose, after too many authentication failures, say),
// and "invalid format" from the banner exchange (whatever answered does
// not speak ssh).

// classifyFailure reads one failure's error text. It reports the
// failure's error.type ("" when the text does not say) and whether the
// failure is expected: ssh failing to reach the remote. Anything else
// is a fault.
func classifyFailure(msg string) (kind string, expected bool) {
	if !strings.Contains(msg, sshExitStatus) {
		return "", false
	}
	for _, p := range sshFaults {
		if strings.Contains(msg, p.phrase) {
			return p.kind, false
		}
	}
	for _, p := range sshWeather {
		if strings.Contains(msg, p.phrase) {
			return p.kind, true
		}
	}
	return "", false
}

// cycleFailure decides how a failed cycle is logged: the level, the
// error.message — the refresh error that aborted the cycle, or each
// failed dataset's error, one per line — and the error.type, "" when
// unclassified. The cycle is WARN only when it has failures and every
// one is expected; the error.type is that of the failure the level was
// decided on: the first fault, or, when there is none, the first
// failure.
func cycleFailure(c history.Cycle) (level slog.Level, message, kind string) {
	var errs, raw []string
	if c.Error != "" {
		errs, raw = append(errs, c.Error), append(raw, c.Error)
	}
	for _, f := range c.Failures {
		errs = append(errs, fmt.Sprintf("syncing '%s': %s", f.Dataset, f.Error))
		raw = append(raw, f.Error)
	}
	message = strings.Join(errs, "\n")
	if len(raw) == 0 {
		return slog.LevelError, message, ""
	}
	level = slog.LevelWarn
	kind, _ = classifyFailure(raw[0])
	for _, e := range raw {
		if k, expected := classifyFailure(e); !expected {
			return slog.LevelError, message, k
		}
	}
	return level, message, kind
}
