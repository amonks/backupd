package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"monks.co/backupd/config"
	"monks.co/backupd/sim"
)

// demoConfig is the config the -sim daemon runs under. It is written to
// a real temp file so the whole config-editing path — pause toggles,
// preview, save-and-apply, hand-edit reload — works exactly as in
// production. The short interval keeps the demo lively.
const demoConfig = `# backupd -sim configuration (temp file; edits apply like production)
interval = "45s"
listen = "127.0.0.1:8899"

[local]
root = "sim/tank"
[local.policy]
hourly = 6
daily = 7

[remote]
ssh_host = "sim"
root = "sim/backup"
[remote.policy]
daily = 14

[overrides."/tm"]
paused = true
keep_baseline = false
[overrides."/tm".local.policy]
daily = 1
[overrides."/tm".remote.policy]
daily = 1
`

// runSim boots the complete daemon — sync loop, planner, dashboard —
// against the sim package's in-memory ZFS pair. No root, ZFS, or
// network required. A churn goroutine takes an hourly snapshot every
// 40 seconds so cycles always have fresh work to look at.
func runSim(ctx context.Context, addr string) error {
	dir, err := os.MkdirTemp("", "backupd-sim-")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "backupd.toml")
	if err := os.WriteFile(path, []byte(demoConfig), 0o644); err != nil {
		return err
	}
	conf, err := config.LoadFrom(path)
	if err != nil {
		return err
	}
	if addr == "" {
		addr = conf.Listen
	}

	s := sim.Demo(time.Now())
	b := NewWithEnv(conf, addr, false, s)
	s.OnProgress = b.activity.Progress

	go func() {
		ticker := time.NewTicker(40 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.Snapshot(ctx, b.globalLogs, conf.Local.Root, "hourly"); err != nil {
					b.globalLogs.Printf("sim churn: %s", err)
				}
			}
		}
	}()

	log.Printf("sim mode: dashboard at http://%s, config at %s", addr, path)
	return b.Go(ctx)
}
