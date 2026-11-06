package daemon

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

// The settings that become part of a command the daemon runs — the
// escalation prefix in front of every local zfs command, the ssh
// destination and key, the datasets the commands name — and the
// address the control API listens on are the operator's, edited in the
// file. The API, whose only gate is that a caller can reach it, may
// not change them: a caller who could would choose what the daemon
// runs at its next start. Saving and previewing are both refused,
// naming the settings, and the file and the running config are left
// alone; anything else in the same file still saves.
func TestTheAPICannotChangeWhatTheDaemonRuns(t *testing.T) {
	for _, tc := range []struct{ setting, edited string }{
		{"local.escalate", strings.Replace(apiTestConfig, `root = "data/tank"`, `root = "data/tank"`+"\nescalate = [\"sh\", \"-c\", \"id\"]", 1)},
		{"local.root", strings.Replace(apiTestConfig, `root = "data/tank"`, `root = "data/other"`, 1)},
		{"remote.root", strings.Replace(apiTestConfig, `root = "backup/tank"`, `root = "backup/tank; id"`, 1)},
		{"remote.ssh_host", strings.Replace(apiTestConfig, `root = "backup/tank"`, `root = "backup/tank"`+"\nssh_host = \"-oProxyCommand=id\"", 1)},
		{"remote.ssh_key", strings.Replace(apiTestConfig, `root = "backup/tank"`, `root = "backup/tank"`+"\nssh_key = \"/tmp/key\"", 1)},
		{"listen", "listen = \"0.0.0.0:8888\"\n" + apiTestConfig},
	} {
		t.Run(tc.setting, func(t *testing.T) {
			local, remote := steadyStateExecutors()
			b, path := newAPITestDaemon(t, local, remote)
			h := b.Handler()

			for _, route := range []struct{ method, path string }{
				{"POST", "/api/config/preview"},
				{"PUT", "/api/config"},
			} {
				w := do(t, h, route.method, route.path, tc.edited)
				if w.Code != http.StatusForbidden {
					t.Errorf("%s %s changing %s: %d, want 403: %s", route.method, route.path, tc.setting, w.Code, w.Body)
				}
				if !strings.Contains(w.Body.String(), tc.setting) {
					t.Errorf("%s %s: refusal does not name %s: %q", route.method, route.path, tc.setting, w.Body)
				}
			}
			onDisk, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(onDisk) != apiTestConfig {
				t.Errorf("the file changed:\n%s", onDisk)
			}
			if got := string(b.conf.Deref().Raw); got != apiTestConfig {
				t.Errorf("the running config changed:\n%s", got)
			}
		})
	}
}

// The comparison is against the file as the operator last left it,
// not the daemon's copy: an operator's hand edit the daemon has not
// reloaded yet must not be reverted by a save that carries the old
// value, and one that carries the operator's new value is no change.
func TestTheAPIComparesAgainstTheFileOnDisk(t *testing.T) {
	local, remote := steadyStateExecutors()
	b, path := newAPITestDaemon(t, local, remote)
	h := b.Handler()

	byHand := strings.Replace(apiTestConfig, `root = "data/tank"`, `root = "data/tank"`+"\nescalate = [\"sudo\", \"-n\"]", 1)
	if err := os.WriteFile(path, []byte(byHand), 0o644); err != nil {
		t.Fatal(err)
	}

	// The dashboard's copy predates the edit: saving it would undo it.
	if w := do(t, h, "PUT", "/api/config", apiTestConfig); w.Code != http.StatusForbidden {
		t.Errorf("a save reverting the operator's escalate: %d, want 403: %s", w.Code, w.Body)
	}
	// Carrying the operator's value, a retention change saves.
	edited := strings.Replace(byHand, "daily = 1", "daily = 7", 1)
	if w := do(t, h, "PUT", "/api/config", edited); w.Code != http.StatusOK {
		t.Fatalf("a retention change keeping the operator's settings: %d: %s", w.Code, w.Body)
	}
	if onDisk, _ := os.ReadFile(path); string(onDisk) != edited {
		t.Errorf("the retention change was not saved:\n%s", onDisk)
	}
}

// Pause state lives in the file, so a toggle edits the file as it is
// on disk: an operator's hand edit the daemon has not reloaded yet —
// an operator-only setting included — survives the toggle, and the
// toggle is not refused for carrying the daemon's older copy of it.
func TestPauseEditsTheFileOnDisk(t *testing.T) {
	local, remote := steadyStateExecutors()
	b, path := newAPITestDaemon(t, local, remote)
	h := b.Handler()

	byHand := strings.Replace(apiTestConfig, `root = "data/tank"`, `root = "data/tank"`+"\nescalate = [\"sudo\", \"-n\"]", 1)
	if err := os.WriteFile(path, []byte(byHand), 0o644); err != nil {
		t.Fatal(err)
	}
	if w := do(t, h, "POST", "/api/pause", ""); w.Code != http.StatusOK {
		t.Fatalf("pause after a hand edit: %d: %s", w.Code, w.Body)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(onDisk), `escalate = ["sudo", "-n"]`) {
		t.Errorf("the toggle reverted the operator's hand edit:\n%s", onDisk)
	}
	if !b.conf.Deref().Paused {
		t.Error("the pause was not applied")
	}
}
