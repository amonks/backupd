//go:build unix

package env

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"monks.co/backupd/logger"
)

// recorder is an Executor that remembers what it was asked to run.
type recorder struct {
	calls [][]string
	rows  []string
}

func (r *recorder) Exec(l *logger.Logger, args ...string) ([]string, error) {
	r.calls = append(r.calls, slices.Clone(args))
	return r.rows, nil
}

func (r *recorder) Execf(l *logger.Logger, s string, args ...any) ([]string, error) {
	return r.Exec(l, strings.Fields(s)...)
}

func (r *recorder) Command(args ...string) *exec.Cmd {
	r.calls = append(r.calls, slices.Clone(args))
	return exec.Command("true")
}

// TestEscalationPrefixesLocalCommands: a daemon running as an ordinary
// user reaches zfs through the prefix its config names, and a daemon
// with no prefix runs zfs directly, exactly as before.
func TestEscalationPrefixesLocalCommands(t *testing.T) {
	for _, tc := range []struct {
		name     string
		escalate []string
		want     []string
	}{
		{"none", nil, []string{"zfs", "send", "tank@daily-1"}},
		{"sudo", []string{"sudo", "-n"}, []string{"sudo", "-n", "zfs", "send", "tank@daily-1"}},
		{"doas", []string{"doas"}, []string{"doas", "zfs", "send", "tank@daily-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &LocalExecutor{Escalate: tc.escalate}
			if got := l.Command("zfs", "send", "tank@daily-1").Args; !slices.Equal(got, tc.want) {
				t.Errorf("Command args = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEscalationDoesNotLeakIntoSizeArgs: Size takes the send's own
// arguments and appends its dry-run flags to them, while the prefix
// belongs to the executor. Getting this backwards would either measure
// the wrong command or hand the prefix a second copy of itself — and
// appending in place would leave --dryrun on the transfer that follows.
func TestEscalationDoesNotLeakIntoSizeArgs(t *testing.T) {
	rec := &recorder{rows: []string{"size\t1024"}}
	zfs := NewZFS("tank", rec)

	send := []string{"zfs", "send", "tank@daily-1"}
	if _, err := zfs.Size(logger.New("test"), send); err != nil {
		t.Fatalf("Size: %v", err)
	}

	want := []string{"zfs", "send", "tank@daily-1", "--dryrun", "--verbose", "--parsable"}
	if !slices.Equal(rec.calls[0], want) {
		t.Errorf("Size ran %v, want %v", rec.calls[0], want)
	}
	if !slices.Equal(send, []string{"zfs", "send", "tank@daily-1"}) {
		t.Errorf("Size mutated its caller's args: %v", send)
	}
}

// TestSizeRefusesNonSendCommands keeps the guard that made the old
// signature safe: --dryrun on something that is not a send would run it
// for real.
func TestSizeRefusesNonSendCommands(t *testing.T) {
	zfs := NewZFS("tank", &recorder{})
	for _, args := range [][]string{nil, {"zfs"}, {"zfs", "destroy", "tank@daily-1"}, {"sudo", "zfs", "send"}} {
		if _, err := zfs.Size(logger.New("test"), args); err == nil {
			t.Errorf("Size(%v) = nil error, want a refusal", args)
		}
	}
}

// TestTerminateKillsWrappedChildren is why an escalation prefix is safe
// to cancel. sudo does not exec its command — it forks and waits — so
// killing the process the daemon started leaves the real worker running,
// reparented to init, still holding the pipe the daemon waits to drain.
// A wrapper that spawns a child stands in for sudo here.
func TestTerminateKillsWrappedChildren(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "child.pid")
	// The shell forks a long sleep, records its pid, and waits — the
	// shape sudo has, without needing sudo in a test.
	cmd := exec.Command("/bin/sh", "-c", "sleep 60 & echo $! > "+pidfile+"; wait")
	isolate(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { terminate(cmd); cmd.Wait() })

	child := waitForPidfile(t, pidfile)
	if !alive(child) {
		t.Fatalf("child %d never came up", child)
	}

	terminate(cmd)
	cmd.Wait()

	// A real-clock bound, not a measurement: the assertion is that the
	// child dies, and the deadline only keeps a failure from hanging.
	deadline := time.Now().Add(5 * time.Second)
	for alive(child) {
		if time.Now().After(deadline) {
			syscall.Kill(child, syscall.SIGKILL)
			t.Fatal("child outlived its parent: terminate did not reach the process group")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForPidfile reads the pid the wrapper recorded, waiting for the
// shell to get that far.
func waitForPidfile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if bs, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(bs))); err == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("wrapper never wrote its child's pid")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// alive reports whether a pid still exists. Signal 0 checks for the
// process without delivering anything.
func alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
