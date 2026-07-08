package config

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

type Config struct {
	SnitchID string `toml:"snitch_id"`
	Remote   struct {
		SSHKey  string         `toml:"ssh_key"`
		SSHHost string         `toml:"ssh_host"`
		Policy  map[string]int `toml:"policy"`
		Root    string         `toml:"root"`
	} `toml:"remote"`
	Local struct {
		Policy map[string]int `toml:"policy"`
		Root   string         `toml:"root"`
	}
	Overrides map[string]*Override `toml:"overrides"`
}

// Override customizes retention for a dataset subtree. Keys are dataset
// paths relative to the root (leading slash optional). A policy given here
// replaces the global policy for that side wholesale; an omitted side falls
// back to the global policy. KeepBaseline defaults to true; setting it to
// false stops preserving the oldest and earliest-shared snapshots, leaving
// only policy matches and the latest shared snapshot (the sync point).
type Override struct {
	KeepBaseline *bool `toml:"keep_baseline"`
	Local        struct {
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
		f, err := os.Open(path)
		if err != nil && os.IsNotExist(err) {
			continue
		} else if err != nil {
			return nil, err
		}

		defer f.Close()

		dec := toml.NewDecoder(f)
		var conf Config
		if _, err := dec.Decode(&conf); err != nil {
			return nil, fmt.Errorf("decoding '%s': %w", path, err)
		}

		return &conf, nil
	}

	return nil, fmt.Errorf("no config file exists {%s}", strings.Join(pathHierarchy, ", "))
}
