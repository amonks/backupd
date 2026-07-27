package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const pauseTestConfig = `# backupd config for thor
snitch_id = "abc123"

[local]
root = "data/tank"
[local.policy]
daily = 90

[remote]
root = "backup/tank"
[remote.policy]
daily = 1

# Time Machine sparsebundles keep their own history.
[overrides."/tm"]
keep_baseline = false
[overrides."/tm".local.policy]
daily = 1

[overrides."music"]
keep_baseline = false
`

func TestParse_Valid(t *testing.T) {
	conf, err := Parse([]byte(pauseTestConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if conf.SnitchID != "abc123" {
		t.Errorf("expected snitch_id to parse, got %q", conf.SnitchID)
	}
	if string(conf.Raw) != pauseTestConfig {
		t.Errorf("expected Raw to hold the source bytes")
	}
}

func TestParse_UnknownKeyRejected(t *testing.T) {
	src := pauseTestConfig + "\n[remote.polcy]\ndialy = 3\n"
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("expected error for unknown key, got nil")
	}
	if !strings.Contains(err.Error(), "polcy") {
		t.Errorf("expected error to name the unknown key, got: %v", err)
	}
}

func TestParse_BadInterval(t *testing.T) {
	src := "interval = \"bogus\"\n" + pauseTestConfig
	if _, err := Parse([]byte(src)); err == nil {
		t.Fatal("expected error for unparseable interval, got nil")
	}
}

func TestInterval_Default(t *testing.T) {
	conf, err := Parse([]byte(pauseTestConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := conf.Interval(); got != time.Hour {
		t.Errorf("expected default interval 1h, got %s", got)
	}
}

func TestInterval_Configured(t *testing.T) {
	src := "interval = \"30m\"\n" + pauseTestConfig
	conf, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := conf.Interval(); got != 30*time.Minute {
		t.Errorf("expected 30m interval, got %s", got)
	}
}

func TestPausedFor_GlobalPause(t *testing.T) {
	src := "paused = true\n" + pauseTestConfig
	conf, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, ds := range []string{"", "/tm", "/movies"} {
		if !conf.PausedFor(ds) {
			t.Errorf("expected %q to be paused under global pause", ds)
		}
	}
}

func TestPausedFor_SubtreePause(t *testing.T) {
	src := strings.Replace(pauseTestConfig,
		"[overrides.\"/tm\"]\nkeep_baseline = false",
		"[overrides.\"/tm\"]\npaused = true\nkeep_baseline = false", 1)
	conf, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !conf.PausedFor("/tm") {
		t.Error("expected /tm to be paused")
	}
	if !conf.PausedFor("/tm/brigid") {
		t.Error("expected /tm/brigid to be paused (subtree)")
	}
	if conf.PausedFor("/tmp") {
		t.Error("expected /tmp to not be paused (component boundary)")
	}
	if conf.PausedFor("") {
		t.Error("expected root to not be paused")
	}
	if conf.PausedFor("/movies") {
		t.Error("expected /movies to not be paused")
	}
}

// Pause accumulates across all matching override prefixes, unlike retention
// resolution where only the longest prefix applies. A dataset with its own
// retention override is still paused when a parent subtree is paused.
func TestPausedFor_AccumulatesAcrossPrefixes(t *testing.T) {
	src := pauseTestConfig + `
[overrides."/tm/lugh"]
[overrides."/tm/lugh".local.policy]
daily = 3
`
	src = strings.Replace(src,
		"[overrides.\"/tm\"]\nkeep_baseline = false",
		"[overrides.\"/tm\"]\npaused = true\nkeep_baseline = false", 1)
	conf, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !conf.PausedFor("/tm/lugh") {
		t.Error("expected /tm/lugh to be paused via the /tm subtree pause, even though it has its own retention override")
	}
}

func TestSubtreePaused_IgnoresGlobal(t *testing.T) {
	src := "paused = true\n" + strings.Replace(pauseTestConfig,
		"[overrides.\"/tm\"]\nkeep_baseline = false",
		"[overrides.\"/tm\"]\npaused = true\nkeep_baseline = false", 1)
	conf, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !conf.SubtreePaused("/tm") {
		t.Error("expected /tm subtree to be paused")
	}
	if conf.SubtreePaused("/movies") {
		t.Error("expected /movies subtree to not be paused despite global pause")
	}
}

func TestSetPaused_GlobalAdd(t *testing.T) {
	out, err := SetPaused([]byte(pauseTestConfig), "", true)
	if err != nil {
		t.Fatalf("SetPaused: %v", err)
	}
	conf, err := Parse(out)
	if err != nil {
		t.Fatalf("Parse after SetPaused: %v", err)
	}
	if !conf.Paused {
		t.Error("expected global paused=true")
	}
	// Comments and existing content must survive.
	for _, want := range []string{"# backupd config for thor", "snitch_id = \"abc123\"", "# Time Machine sparsebundles keep their own history."} {
		if !strings.Contains(string(out), want) {
			t.Errorf("expected output to preserve %q\n---\n%s", want, out)
		}
	}
	// The paused line must be top-level: before the first section header.
	idx := strings.Index(string(out), "paused = true")
	firstSection := strings.Index(string(out), "[")
	if idx == -1 || firstSection == -1 || idx > firstSection {
		t.Errorf("expected top-level paused line before first section\n---\n%s", out)
	}
}

func TestSetPaused_GlobalRemove(t *testing.T) {
	paused, err := SetPaused([]byte(pauseTestConfig), "", true)
	if err != nil {
		t.Fatalf("SetPaused(true): %v", err)
	}
	out, err := SetPaused(paused, "", false)
	if err != nil {
		t.Fatalf("SetPaused(false): %v", err)
	}
	conf, err := Parse(out)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if conf.Paused {
		t.Error("expected global paused=false")
	}
	if strings.Contains(string(out), "paused") {
		t.Errorf("expected paused line to be removed\n---\n%s", out)
	}
}

func TestSetPaused_GlobalUpdateExisting(t *testing.T) {
	src := "paused = false\n" + pauseTestConfig
	out, err := SetPaused([]byte(src), "", true)
	if err != nil {
		t.Fatalf("SetPaused: %v", err)
	}
	if got := strings.Count(string(out), "paused"); got != 1 {
		t.Errorf("expected exactly one paused line, got %d\n---\n%s", got, out)
	}
	conf, err := Parse(out)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !conf.Paused {
		t.Error("expected paused=true")
	}
}

func TestSetPaused_DatasetExistingSection(t *testing.T) {
	out, err := SetPaused([]byte(pauseTestConfig), "/tm", true)
	if err != nil {
		t.Fatalf("SetPaused: %v", err)
	}
	conf, err := Parse(out)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !conf.PausedFor("/tm") {
		t.Error("expected /tm to be paused")
	}
	if conf.Paused {
		t.Error("expected global to stay unpaused")
	}
	// The override's other settings must be intact.
	_, _, keepBaseline := conf.PolicyFor("/tm")
	if keepBaseline {
		t.Error("expected keep_baseline=false to survive")
	}
	local, _, _ := conf.PolicyFor("/tm")
	if local["daily"] != 1 {
		t.Errorf("expected /tm local policy to survive, got %v", local)
	}
}

func TestSetPaused_DatasetNewSection(t *testing.T) {
	out, err := SetPaused([]byte(pauseTestConfig), "/movies", true)
	if err != nil {
		t.Fatalf("SetPaused: %v", err)
	}
	conf, err := Parse(out)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !conf.PausedFor("/movies") {
		t.Error("expected /movies to be paused via new override section")
	}
	// A pause-only override must not disturb retention.
	local, _, keepBaseline := conf.PolicyFor("/movies")
	if local["daily"] != 90 || !keepBaseline {
		t.Errorf("expected global retention for /movies, got %v keepBaseline=%v", local, keepBaseline)
	}
}

func TestSetPaused_DatasetRemove(t *testing.T) {
	paused, err := SetPaused([]byte(pauseTestConfig), "/tm", true)
	if err != nil {
		t.Fatalf("SetPaused(true): %v", err)
	}
	out, err := SetPaused(paused, "/tm", false)
	if err != nil {
		t.Fatalf("SetPaused(false): %v", err)
	}
	conf, err := Parse(out)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if conf.PausedFor("/tm") {
		t.Error("expected /tm to be unpaused")
	}
	_, _, keepBaseline := conf.PolicyFor("/tm")
	if keepBaseline {
		t.Error("expected keep_baseline=false to survive the round trip")
	}
}

// The override key may be written without a leading slash; SetPaused
// callers always pass the normalized (leading-slash) dataset path.
func TestSetPaused_KeyNormalization(t *testing.T) {
	paused, err := SetPaused([]byte(pauseTestConfig), "/music", true)
	if err != nil {
		t.Fatalf("SetPaused: %v", err)
	}
	conf, err := Parse(paused)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !conf.PausedFor("/music") {
		t.Error("expected /music to be paused via the existing 'music' section")
	}
	// It must have edited the existing section, not created a duplicate.
	if got := strings.Count(string(paused), "music"); got != 1 {
		t.Errorf("expected exactly one music section, got %d\n---\n%s", got, paused)
	}
}

func TestSave_Atomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backupd.toml")
	if err := os.WriteFile(path, []byte(pauseTestConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	updated := "paused = true\n" + pauseTestConfig
	if err := Save(path, []byte(updated)); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != updated {
		t.Errorf("expected saved content to match")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("expected file mode to be preserved, got %v", info.Mode().Perm())
	}
}

func TestLoadFrom(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backupd.toml")
	if err := os.WriteFile(path, []byte(pauseTestConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	conf, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if conf.Path != path {
		t.Errorf("expected Path to be recorded, got %q", conf.Path)
	}
	if string(conf.Raw) != pauseTestConfig {
		t.Error("expected Raw to hold file contents")
	}
}
