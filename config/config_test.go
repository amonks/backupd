package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

const testConfig = `
[local]
root = "data/tank"
[local.policy]
daily = 90
monthly = 12

[remote]
root = "backup/tank"
[remote.policy]
daily = 1
monthly = 6

[overrides."/tm"]
keep_baseline = false
[overrides."/tm".local.policy]
daily = 1
[overrides."/tm".remote.policy]
daily = 2

[overrides."/tm/lugh"]
[overrides."/tm/lugh".local.policy]
daily = 3

[overrides."music"]
keep_baseline = false
`

func loadTestConfig(t *testing.T) *Config {
	t.Helper()
	var conf Config
	if _, err := toml.Decode(testConfig, &conf); err != nil {
		t.Fatalf("decoding test config: %v", err)
	}
	return &conf
}

func TestPolicyFor_Default(t *testing.T) {
	conf := loadTestConfig(t)

	local, remote, keepBaseline := conf.PolicyFor("/movies")
	if local["daily"] != 90 || local["monthly"] != 12 {
		t.Errorf("expected global local policy, got %v", local)
	}
	if remote["daily"] != 1 || remote["monthly"] != 6 {
		t.Errorf("expected global remote policy, got %v", remote)
	}
	if !keepBaseline {
		t.Errorf("expected keepBaseline=true by default")
	}
}

func TestPolicyFor_Root(t *testing.T) {
	conf := loadTestConfig(t)

	local, _, keepBaseline := conf.PolicyFor("")
	if local["daily"] != 90 {
		t.Errorf("expected global local policy for root, got %v", local)
	}
	if !keepBaseline {
		t.Errorf("expected keepBaseline=true for root")
	}
}

func TestPolicyFor_OverrideExact(t *testing.T) {
	conf := loadTestConfig(t)

	local, remote, keepBaseline := conf.PolicyFor("/tm")
	if local["daily"] != 1 || len(local) != 1 {
		t.Errorf("expected override local policy {daily:1}, got %v", local)
	}
	if remote["daily"] != 2 || len(remote) != 1 {
		t.Errorf("expected override remote policy {daily:2}, got %v", remote)
	}
	if keepBaseline {
		t.Errorf("expected keepBaseline=false from override")
	}
}

func TestPolicyFor_OverrideSubtree(t *testing.T) {
	conf := loadTestConfig(t)

	local, remote, keepBaseline := conf.PolicyFor("/tm/brigid")
	if local["daily"] != 1 || remote["daily"] != 2 || keepBaseline {
		t.Errorf("expected /tm override to apply to /tm/brigid, got local=%v remote=%v keepBaseline=%v",
			local, remote, keepBaseline)
	}
}

func TestPolicyFor_ComponentBoundary(t *testing.T) {
	conf := loadTestConfig(t)

	// "/tmp" shares a string prefix with "/tm" but is a different dataset.
	local, _, keepBaseline := conf.PolicyFor("/tmp")
	if local["daily"] != 90 || !keepBaseline {
		t.Errorf("expected /tmp to get global policy, got %v keepBaseline=%v", local, keepBaseline)
	}
}

func TestPolicyFor_LongestPrefixWins(t *testing.T) {
	conf := loadTestConfig(t)

	// "/tm/lugh" has its own override; it wins over "/tm" and does not
	// inherit from it. Omitted sides fall back to the globals.
	local, remote, keepBaseline := conf.PolicyFor("/tm/lugh")
	if local["daily"] != 3 {
		t.Errorf("expected /tm/lugh local override, got %v", local)
	}
	if remote["daily"] != 1 || remote["monthly"] != 6 {
		t.Errorf("expected global remote policy (no inheritance from /tm), got %v", remote)
	}
	if !keepBaseline {
		t.Errorf("expected keepBaseline=true (omitted in /tm/lugh override)")
	}
}

func TestOverrideFor(t *testing.T) {
	conf := loadTestConfig(t)

	if key, o := conf.OverrideFor("/movies"); o != nil {
		t.Errorf("expected no override for /movies, got %q", key)
	}
	if key, o := conf.OverrideFor("/tm/brigid"); o == nil || key != "/tm" {
		t.Errorf("expected /tm override for /tm/brigid, got %q %v", key, o)
	}
	if key, o := conf.OverrideFor("/tm/lugh"); o == nil || key != "/tm/lugh" {
		t.Errorf("expected /tm/lugh override (longest prefix), got %q %v", key, o)
	}
}

func TestRetentionDescription(t *testing.T) {
	conf := loadTestConfig(t)

	if got := conf.RetentionDescription("/movies"); got != "default" {
		t.Errorf("expected 'default', got %q", got)
	}

	want := "override /tm: local {daily=1}, remote {daily=2}, no baseline"
	if got := conf.RetentionDescription("/tm/brigid"); got != want {
		t.Errorf("expected %q, got %q", want, got)
	}

	want = "override /tm/lugh: local {daily=3}"
	if got := conf.RetentionDescription("/tm/lugh"); got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

func TestPolicyFor_KeyNormalization(t *testing.T) {
	conf := loadTestConfig(t)

	// Override keys may be written without a leading slash.
	local, _, keepBaseline := conf.PolicyFor("/music/mp3")
	if keepBaseline {
		t.Errorf("expected 'music' override to match /music/mp3")
	}
	// The override defines no policies, so globals apply.
	if local["daily"] != 90 {
		t.Errorf("expected global local policy, got %v", local)
	}
}

// TestPathHierarchyPrefersADirectoryOfItsOwn: saving a config is a
// write to a temp file and a rename over the target, which needs write
// permission on the directory. A daemon that is not root — the point of
// local.escalate — therefore cannot pause itself with its config in a
// root-owned /usr/local/etc, so the directory forms are searched first
// and can be owned by whoever runs the daemon.
func TestPathHierarchyPrefersADirectoryOfItsOwn(t *testing.T) {
	var flatFirst, dirLast int
	for i, path := range pathHierarchy {
		if strings.HasSuffix(path, "/backupd/backupd.toml") {
			dirLast = i
		} else if flatFirst == 0 {
			flatFirst = i
		}
	}
	if dirLast > flatFirst {
		t.Errorf("a flat path is searched before a directory one:\n%v", pathHierarchy)
	}
	for _, want := range []string{"/etc/backupd/backupd.toml", "/usr/local/etc/backupd/backupd.toml"} {
		if !slices.Contains(pathHierarchy, want) {
			t.Errorf("%s is not searched", want)
		}
	}
}
