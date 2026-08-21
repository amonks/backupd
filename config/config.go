package config

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	SnitchID string `toml:"snitch_id"`
	// Paused pauses execution globally: state is still refreshed and plans
	// are still generated, but no transfers or deletions run.
	Paused bool `toml:"paused"`
	// IntervalRaw is the duration between sync cycles (default "1h").
	IntervalRaw string `toml:"interval"`
	// Listen is the HTTP listen address (default "0.0.0.0:8888"); the
	// -addr flag overrides it.
	Listen string `toml:"listen"`
	Remote struct {
		SSHKey  string         `toml:"ssh_key"`
		SSHHost string         `toml:"ssh_host"`
		Policy  map[string]int `toml:"policy"`
		Root    string         `toml:"root"`
	} `toml:"remote"`
	Local struct {
		Policy map[string]int `toml:"policy"`
		Root   string         `toml:"root"`
		// Escalate is a command prefix applied to every local ZFS
		// command — ["sudo", "-n"], ["doas"] — so the daemon can run
		// as an ordinary user on a host where only root may drive
		// zfs. Empty (the default) runs zfs directly, which requires
		// the daemon itself to be root. Setting it is also what
		// relaxes the CLI's own root check: a daemon that escalates
		// per command is not supposed to be root.
		//
		// The remote side takes no prefix: it already arrives over
		// ssh as whichever user the key authenticates as.
		Escalate []string `toml:"escalate"`
	} `toml:"local"`
	Overrides map[string]*Override `toml:"overrides"`

	// Path is the file this config was loaded from, and Raw its exact
	// bytes. Raw, not a re-serialization, is the source of truth for
	// editing: it preserves comments and formatting.
	Path string `toml:"-"`
	Raw  []byte `toml:"-"`
}

// Override customizes retention for a dataset subtree. Keys are dataset
// paths relative to the root (leading slash optional). A policy given here
// replaces the global policy for that side wholesale; an omitted side falls
// back to the global policy. KeepBaseline defaults to true; setting it to
// false stops preserving the oldest and earliest-shared snapshots, leaving
// only policy matches and the latest shared snapshot (the sync point).
type Override struct {
	KeepBaseline *bool `toml:"keep_baseline"`
	// Paused pauses execution for this subtree. Unlike retention, pause is
	// cumulative: a dataset is paused if any matching override prefix is
	// paused, regardless of which override wins retention resolution.
	Paused bool `toml:"paused"`
	Local  struct {
		Policy map[string]int `toml:"policy"`
	} `toml:"local"`
	Remote struct {
		Policy map[string]int `toml:"policy"`
	} `toml:"remote"`
}

// OverrideFor returns the override that applies to a dataset (named
// relative to the root with a leading slash; "" is the root itself) along
// with its normalized key, or ("", nil) if none matches. The
// longest-prefix-matching override wins; overrides do not inherit from
// each other.
func (c *Config) OverrideFor(dataset string) (string, *Override) {
	var bestKey string
	var best *Override
	for key, override := range c.Overrides {
		key = normalizeOverrideKey(key)
		if dataset != key && !strings.HasPrefix(dataset, key+"/") {
			continue
		}
		if best == nil || len(key) > len(bestKey) {
			bestKey, best = key, override
		}
	}
	return bestKey, best
}

// PolicyFor resolves the retention policies and baseline behavior for a
// dataset via OverrideFor, falling back to the global policies.
func (c *Config) PolicyFor(dataset string) (local, remote map[string]int, keepBaseline bool) {
	local, remote, keepBaseline = c.Local.Policy, c.Remote.Policy, true

	_, best := c.OverrideFor(dataset)
	if best == nil {
		return local, remote, keepBaseline
	}

	if best.Local.Policy != nil {
		local = best.Local.Policy
	}
	if best.Remote.Policy != nil {
		remote = best.Remote.Policy
	}
	if best.KeepBaseline != nil {
		keepBaseline = *best.KeepBaseline
	}
	return local, remote, keepBaseline
}

// PausedFor reports whether execution is paused for a dataset (named
// relative to the root with a leading slash; "" is the root itself).
// Pause accumulates across all matching override prefixes rather than
// following retention's longest-prefix-wins rule: pausing /tm pauses
// /tm/lugh even if /tm/lugh has its own retention override.
func (c *Config) PausedFor(dataset string) bool {
	return c.Paused || c.SubtreePaused(dataset)
}

// SubtreePaused reports whether a dataset is paused by an override (its
// own or an ancestor's), ignoring the global pause. The UI uses this to
// decide whether a dataset's button should say Pause or Resume: under a
// global pause, per-dataset buttons still control the subtree flag.
func (c *Config) SubtreePaused(dataset string) bool {
	for key, override := range c.Overrides {
		key = normalizeOverrideKey(key)
		if dataset != key && !strings.HasPrefix(dataset, key+"/") {
			continue
		}
		if override.Paused {
			return true
		}
	}
	return false
}

// Interval returns the duration between sync cycles, defaulting to an
// hour. Parse guarantees IntervalRaw is valid.
func (c *Config) Interval() time.Duration {
	if c.IntervalRaw == "" {
		return time.Hour
	}
	d, err := time.ParseDuration(c.IntervalRaw)
	if err != nil {
		return time.Hour
	}
	return d
}

// RetentionDescription returns a human-readable summary of the retention
// configuration that applies to a dataset, for display in the UI:
// "default", or e.g. "override /tm: local {daily=1}, remote {daily=1}, no
// baseline".
func (c *Config) RetentionDescription(dataset string) string {
	key, o := c.OverrideFor(dataset)
	if o == nil {
		return "default"
	}
	var parts []string
	if o.Local.Policy != nil {
		parts = append(parts, "local "+formatPolicy(o.Local.Policy))
	}
	if o.Remote.Policy != nil {
		parts = append(parts, "remote "+formatPolicy(o.Remote.Policy))
	}
	if o.KeepBaseline != nil && !*o.KeepBaseline {
		parts = append(parts, "no baseline")
	}
	if len(parts) == 0 {
		return fmt.Sprintf("override %s", key)
	}
	return fmt.Sprintf("override %s: %s", key, strings.Join(parts, ", "))
}

func formatPolicy(policy map[string]int) string {
	keys := make([]string, 0, len(policy))
	for k := range policy {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, len(keys))
	for i, k := range keys {
		pairs[i] = fmt.Sprintf("%s=%d", k, policy[k])
	}
	return "{" + strings.Join(pairs, " ") + "}"
}

func normalizeOverrideKey(key string) string {
	key = strings.TrimSuffix(key, "/")
	if !strings.HasPrefix(key, "/") {
		key = "/" + key
	}
	return key
}

var pathHierarchy = []string{
	"/etc/backupd.toml",
	"/usr/local/etc/backupd.toml",
	"/opt/local/etc/backupd.toml",
	"/Library/Application Support/co.monks.backupd/backupd.toml",
}

func Load() (*Config, error) {
	for _, path := range pathHierarchy {
		if _, err := os.Stat(path); err != nil && os.IsNotExist(err) {
			continue
		} else if err != nil {
			return nil, err
		}
		return LoadFrom(path)
	}

	return nil, fmt.Errorf("no config file exists {%s}", strings.Join(pathHierarchy, ", "))
}

// LoadFrom loads and validates the config file at path, recording the
// path and raw bytes for later editing.
func LoadFrom(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	conf, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("decoding '%s': %w", path, err)
	}
	conf.Path = path
	return conf, nil
}

// Parse decodes and validates a config. Decoding is strict: unknown keys
// are an error, so typos like "dialy" surface immediately instead of
// silently retaining nothing.
func Parse(raw []byte) (*Config, error) {
	var conf Config
	meta, err := toml.Decode(string(raw), &conf)
	if err != nil {
		return nil, err
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("unknown keys: %s", strings.Join(keys, ", "))
	}
	if conf.IntervalRaw != "" {
		if _, err := time.ParseDuration(conf.IntervalRaw); err != nil {
			return nil, fmt.Errorf("invalid interval %q: %w", conf.IntervalRaw, err)
		}
	}
	conf.Raw = raw
	return &conf, nil
}

// Save atomically replaces the config file at path with raw, preserving
// the file's mode. Write-then-rename ensures the daemon never loads a
// half-written config after a crash.
func Save(path string, raw []byte) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, mode); err != nil {
		return fmt.Errorf("writing '%s': %w", tmp, err)
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return fmt.Errorf("chmod '%s': %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("renaming '%s' to '%s': %w", tmp, path, err)
	}
	return nil
}
