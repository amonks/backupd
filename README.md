# backupd

A daemon for managing ZFS snapshot backups with local and remote targets.

## Overview

`backupd` is a service that manages ZFS snapshots across local and remote systems. It provides automated snapshot creation, replication, and retention policy enforcement to ensure your data is safely backed up according to configurable policies.

Key features:
- Automated ZFS snapshot replication with intelligent planning
- Type-based retention policies with configurable limits
- Real-time web dashboard built around operator questions. Assurance
  is derived from ground truth: every dataset shows when it was last
  snapshotted and last backed up (the recovery point) straight from the
  snapshot inventory, so the answers survive restarts. The overview
  leads with a typed issue list — failing syncs, never-replicated or
  stale backups, stalled snapshotting, cycle failures, an overdue
  snitch, forgotten pauses — each with severity, error detail, and its
  remedy button inline, or an explicit "all clear". A status strip
  shows the system verdict (ok / attention / failing / paused) and a
  live activity line (cycle position "dataset 4 of 12", plan step,
  transfer progress with throughput and ETA, next-cycle countdown).
  Dataset pages answer "what could I recover?" in plain language and
  compare retention policy against what each side actually holds
- Pause and resume — globally or per dataset subtree — persisted in the config file
- Config editing from the dashboard, with validation and a per-dataset impact preview before anything is written
- Hot config reload: retention, pause, and interval changes apply without a restart
- Sync-on-demand (full cycle or single dataset) via UI, API, or CLI
- History built for long runtimes: cycle outcomes collapsed into
  same-outcome runs ("ok × 300 over 12d") with a strip of recent
  checks, per-dataset last success *and* last failure (with the
  error), a searchable executed-operation feed, last snitch ping, and
  a journal that records incidents rather than states — one entry
  when a dataset starts failing, updated in place with a count and
  the latest error while the failure persists, one entry when it
  recovers — so a healthy month stays quiet and a broken week reads
  like an incident report. The dashboard tables (fleet, journal,
  activity, cycle runs, snapshots) are data grids with search,
  faceted filters, typed sorting, and pagination
- Resumable transfers with live progress tracking
- RESTful API for control and state inspection
- Dead Man's Snitch integration for external monitoring
- Dry-run mode for safe testing
- Simulation mode (`backupd -sim`): the full daemon against an
  in-memory ZFS pair — no root, ZFS, or network — for demos and testing
- Atomic state management for consistency

### How It Works

**Core Functionality:**

1. **Automatic Sync Cycle (Hourly):**
   - Discovers all datasets under configured local and remote roots
   - Refreshes snapshot inventories from both locations
   - Calculates target state based on retention policies
   - Generates and validates execution plan
   - Executes transfers and deletions to reach target state
   - Reports status to monitoring services
   - Retries failed cycles in-process with exponential backoff (1m
     doubling to a 30m cap), so a remote outage never kills the daemon
     or the web UI; the backoff resets after a successful cycle

2. **Snapshot Retention Management:**
   - Applies per-type retention policies (e.g., keep 24 hourly, 7 daily)
   - Supports per-dataset-subtree policy overrides (longest prefix wins)
   - Processes snapshots by creation time, keeping newest first
   - Preserves critical snapshots:
     - Oldest snapshot at each location (historical baseline; can be
       disabled per subtree with `keep_baseline = false`)
     - Earliest shared snapshot (incremental transfer base; also governed
       by `keep_baseline`)
     - Latest shared snapshot (synchronization point; always preserved)
   - Deletes non-policy snapshots unless they're critical

3. **Intelligent Transfer Planning:**
   - Only transfers snapshots matching remote retention policy
   - Uses incremental transfers when possible (requires common snapshot)
   - Handles initial transfers for empty remote datasets
   - Skips transfers for snapshots older than remote's newest
   - Groups adjacent snapshots into range operations for efficiency

4. **Operation Types:**
   - **InitialSnapshotTransfer**: First snapshot to empty remote
   - **SnapshotRangeTransfer**: Incremental transfer between two snapshots
   - **SnapshotDeletion**: Remove single snapshot
   - **SnapshotRangeDeletion**: Remove range of snapshots (e.g., `@snap1%snap5`)

5. **Progress and State Management:**
   - Thread-safe state updates using atomic operations
   - Per-dataset progress tracking with operation logs
   - Plan validation before execution (simulated apply)
   - Resumable transfers using ZFS receive tokens
   - Dry-run mode for testing without modifications

## Requirements

- Root, or a way to escalate: driving zfs takes privilege, so `backupd`
  runs as root by default. Set `local.escalate` (below) to run it as an
  ordinary user that escalates per command instead
- ZFS filesystem
- FreeBSD or Linux operating system
- SSH access to remote backup server (if using remote backups)
- Go 1.21+ (for building from source)

## Installation

### Build from source

```bash
# Clone the repository
git clone https://github.com/yourusername/backupd.git
cd backupd

# Build the binary
go build -o backupd

# Install to system path (optional)
sudo cp backupd /usr/local/bin/
sudo chmod +x /usr/local/bin/backupd

# Create log directory
sudo mkdir -p /var/log
```

## Configuration

Configuration is loaded from one of the following locations (in order of precedence):
- `/etc/backupd/backupd.toml`
- `/usr/local/etc/backupd/backupd.toml`
- `/opt/local/etc/backupd/backupd.toml`
- `/etc/backupd.toml`
- `/usr/local/etc/backupd.toml`
- `/opt/local/etc/backupd.toml`
- `/Library/Application Support/co.monks.backupd/backupd.toml`

The directory forms come first and exist for a daemon that is not root.
Pausing and saving a config *edit the config file*, atomically — a write
to a temp file and a rename — which needs write permission on the
directory, not just on the file. A daemon running under `local.escalate`
should be given a directory of its own, owned by whoever runs it, or its
pause button returns a permission error.

### Configuration Structure

```toml
# Optional: External monitoring via Dead Man's Snitch
# Get your snitch ID from https://deadmanssnitch.com
snitch_id = "your-snitch-id"

# Optional: pause all execution. State is still refreshed and plans are
# still generated (the dashboard shows what would happen), but no
# transfers or deletions run, and the snitch ping is withheld so a
# forgotten pause eventually trips the dead man's switch. The dashboard's
# pause button edits this setting.
#paused = true

# Optional: duration between sync cycles (default "1h")
#interval = "1h"

# Optional: HTTP listen address (default "0.0.0.0:8888"). Bind to a
# tailnet/VPN address to restrict access to the dashboard and its
# control API.
#listen = "0.0.0.0:8888"

[local]
# Root dataset to backup (all child datasets included)
root = "tank/data"

# Optional: a command prefix for every local zfs command, so the daemon
# can run as an ordinary user instead of root. Omitted, zfs runs
# directly and the daemon must be root.
#escalate = ["sudo", "-n"]

# Retention policy: how many snapshots of each type to keep locally
# Format: type = count
[local.policy]
hourly = 24      # Keep 24 most recent hourly snapshots (1 day)
daily = 7        # Keep 7 most recent daily snapshots (1 week)
weekly = 4       # Keep 4 most recent weekly snapshots (1 month)
monthly = 12     # Keep 12 most recent monthly snapshots (1 year)
yearly = 5       # Keep 5 most recent yearly snapshots

[remote]
# SSH connection details for remote backup server
ssh_key = "/home/user/.ssh/backup_key"    # Path to SSH private key
ssh_host = "user@backup-server.example.com"  # SSH connection string
root = "tank/backups"                     # Remote dataset root

# Retention policy for remote location
# Typically more conservative than local to save space
[remote.policy]
hourly = 0       # Don't keep hourly on remote
daily = 7        # Keep 7 most recent daily snapshots
weekly = 4       # Keep 4 most recent weekly snapshots
monthly = 6      # Keep 6 most recent monthly snapshots
yearly = 2       # Keep 2 most recent yearly snapshots

# Optional: per-dataset overrides. Keys are dataset paths relative to the
# root (leading slash optional) and match the whole subtree; the longest
# matching prefix wins, and overrides do not inherit from each other. A
# policy given here replaces the global policy for that side wholesale;
# an omitted side falls back to the global policy.
#
# keep_baseline = false disables the oldest/earliest-shared snapshot
# preservation for the subtree, leaving only policy matches and the latest
# shared snapshot (the incremental sync point). With a policy like
# {daily = 1}, this yields "keep the latest backed up, but no history":
# each day's snapshot transfers, becomes the new sync point, and its
# predecessor is deleted from both locations on the next cycle.
#
# paused = true pauses execution for the subtree. Unlike retention, pause
# accumulates across matching prefixes: pausing /a pauses /a/b even if
# /a/b has its own retention override.
[overrides."/scratch"]
keep_baseline = false
[overrides."/scratch".local.policy]
daily = 1
[overrides."/scratch".remote.policy]
daily = 1
```

### Example Configurations

<details>
<summary><b>Minimal Local-Only Configuration</b></summary>

```toml
[local]
root = "zpool/data"

[local.policy]
daily = 30
weekly = 8
monthly = 12

# Empty remote section disables remote backups
[remote]
root = ""
```
</details>

<details>
<summary><b>Production Configuration with Monitoring</b></summary>

```toml
snitch_id = "abc123def456"

[local]
root = "production/data"

[local.policy]
hourly = 48
daily = 14
weekly = 8
monthly = 12
yearly = 7

[remote]
ssh_key = "/root/.ssh/backup_rsa"
ssh_host = "backup@192.168.1.100"
root = "backup/production"

[remote.policy]
daily = 7
weekly = 4
monthly = 12
yearly = 5
```
</details>

## Usage

### Command Line Arguments

- `-debug <dataset>`: Debug a specific dataset (performs refresh and plan but no transfers)
- `-logfile <path>`: Log to a file instead of stdout (recommended for production)
- `-addr <address>`: Server address for the web interface (overrides the config `listen` setting; default: "0.0.0.0:8888")
- `-dryrun`: Refresh state but don't execute transfers or deletions (preview mode)
- `-sim`: Run the complete daemon against a simulated in-memory ZFS
  pair (no root, ZFS, or network required). Boots a demo scenario with
  backlogged, in-sync, paused, and never-transferred datasets, paced
  transfers with visible progress, and an interrupted transfer that
  resumes — then serves the ordinary dashboard at 127.0.0.1:8899

### Subcommands

Subcommands are thin wrappers over the running daemon's control API:

```bash
backupd snapshot <periodicity>   # Create a recursive snapshot
backupd pause [dataset]          # Pause execution (all, or one subtree)
backupd resume [dataset]         # Resume execution (all, or one subtree)
backupd sync [dataset]           # Sync now (full cycle, or one dataset)
```

### Basic Usage

```bash
# Run as a service (default mode)
sudo backupd

# Run with logging to file (recommended for production)
sudo backupd -logfile /var/log/backupd.log

# Run in dry-run mode (preview changes without executing)
sudo backupd -dryrun

# Debug a specific dataset (shows plan without executing)
sudo backupd -debug tank/dataset

# Run on custom port
sudo backupd -addr 127.0.0.1:9999

# Create a snapshot via API
curl -X POST "http://localhost:8888/api/snapshot?periodicity=daily"

# Pause the /tm subtree, then resume it
sudo backupd pause /tm
sudo backupd resume /tm
```

### Setting Up as a Daemon

For production use, you'll want to set up `backupd` as a system daemon that starts automatically.

<details>
<summary><b>FreeBSD RC Script</b></summary>

Create the file `/usr/local/etc/rc.d/backupd` with these contents:

```sh
#!/bin/sh
#
# PROVIDE: backupd
# REQUIRE: networking
# KEYWORD:

. /etc/rc.subr

name="backupd"
rcvar="backupd_enable"
backupd_command="/path/to/backupd -logfile=/var/log/backupd.log"
pidfile="/var/run/backupd/${name}.pid"
command="/usr/sbin/daemon"
command_args="-P ${pidfile} -r -f ${backupd_command}"

load_rc_config $name
: ${backupd_enable:=no}

run_rc_command "$1"
```

Make the script executable:
```bash
chmod +x /usr/local/etc/rc.d/backupd
```

Enable and start the service:
```bash
# Add to /etc/rc.conf
echo 'backupd_enable="YES"' >> /etc/rc.conf

# Start the service
service backupd start
```
</details>

<details>
<summary><b>Linux Systemd Service</b></summary>

Create the file `/etc/systemd/system/backupd.service` with these contents:

```ini
[Unit]
Description=Backup Daemon for ZFS Snapshots
After=network.target zfs.target

[Service]
Type=simple
User=root
ExecStart=/path/to/backupd -logfile=/var/log/backupd.log
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
```

Enable and start the service:
```bash
# Reload systemd to recognize the new service
systemctl daemon-reload

# Enable service to start on boot
systemctl enable backupd

# Start the service now
systemctl start backupd

# Check status
systemctl status backupd
```
</details>

## Web Interface and API

### Web UI Endpoints

The web interface provides real-time monitoring and control:
- **Global view**: http://localhost:8888/global - Overview of all datasets, cycle history, and global controls (pause/resume, sync now, snapshot)
- **Config editor**: http://localhost:8888/config - Edit the config file with validation and a per-dataset impact preview before saving
- **Root dataset**: http://localhost:8888/root - Root dataset status
- **Specific dataset**: http://localhost:8888/dataset-name - Individual dataset details, with per-dataset pause and sync-now controls
- **Automatic redirect**: http://localhost:8888/ → /global

The dashboard is unauthenticated: anyone who can reach the listen
address can pause backups and edit the config. Use the `listen` config
setting to bind to a private (e.g. VPN/tailnet) address.

### REST API Endpoints

| Method | Path                        | Description |
|--------|-----------------------------|-------------|
| POST   | `/api/pause?dataset=`       | Pause execution; omit `dataset` for a global pause. Persisted to the config file. |
| POST   | `/api/resume?dataset=`      | Resume execution; omit `dataset` to clear the global pause. |
| POST   | `/api/sync?dataset=`        | Sync now; omit `dataset` to start a full cycle immediately. |
| POST   | `/api/snapshot?periodicity=`| Create a recursive snapshot of the local root. |
| GET    | `/api/config`               | The raw config file (TOML). |
| POST   | `/api/config/preview`       | Validate a config (request body) and report its per-dataset impact without writing. |
| PUT    | `/api/config`               | Validate, atomically save, and hot-apply a config (request body). |
| GET    | `/api/state`                | JSON state summary, serialized from the same derivation layer the HTML renders: system verdict and issues, activity with cycle progress, per-dataset health/ages/fulfillment, cycle history. |

Pause semantics: a paused dataset (or a globally paused daemon) is still
refreshed and replanned every cycle — the dashboard keeps showing what
*would* happen — but no transfers or deletions execute. A pause arriving
mid-plan takes effect at the next step boundary: the in-flight ZFS
operation finishes, and remaining steps stay pending. While globally
paused, the Dead Man's Snitch ping is withheld, so a forgotten pause
eventually raises an external alert.

Config editing: `PUT /api/config` validates strictly (unknown keys are
rejected, so typos like `dialy` surface immediately), writes atomically
(temp file + rename), and applies the result in memory — retention,
pause, interval, and snitch changes take effect without a restart.
Changes to the local/remote roots or SSH endpoint still require a
restart, and are logged as such. Hand-edits to the config file are
picked up at the start of each cycle.

## Snapshot Naming Format and Policy Resolution

For `backupd` to properly apply retention policies, snapshots must follow this naming convention:

```
dataset@type-label
```

Snapshot names have two parts separated by a hyphen:
- `type`: Used to match against policy configuration (e.g., "hourly", "daily")
- `label`: An arbitrary suffix that can be anything you choose

### Important Details:

1. **Policy Type Extraction**: The system extracts everything before the first hyphen as the snapshot's "type". This type is matched against your policy configuration to determine which snapshots to keep.

2. **Custom Policy Types**: The policy types in your configuration are not reserved keywords. You can define any categories (not just "hourly", "daily", etc.), and as long as your snapshot names begin with those types, the system will apply retention policies accordingly. Snapshots with types that don't match any configured policy will not be included in the retention policy calculations, but some may still be preserved for continuity reasons (such as oldest snapshots and shared snapshots between local and remote).

3. **Snapshot Ordering**: Snapshots are ordered by their actual creation timestamp (`CreatedAt`), not by the name. The snapshot name is only used to determine the type.

4. **Policy Application**: When applying policies, `backupd` processes the newest snapshots first (based on creation time) and keeps the specified number of each type.

5. **Examples**:
   - If your policy defines `hourly = 24`, snapshots named `dataset@hourly-anything` will be retained (24 newest ones)
   - You could define a policy with `critical = 10` and name snapshots `dataset@critical-backup1`

6. **Handling Non-Policy Snapshots**: For snapshots with types that don't match any policy configuration:
   - They won't be included in policy-based retention calculations
   - They won't automatically be transferred to remote storage
   - Some may still be preserved if they are:
     - The oldest snapshot (locally or remotely)
     - The earliest or latest snapshot shared between local and remote
   - This allows you to have manual or special-purpose snapshots that won't be automatically managed

### Recommended Snapshot Regime

A good snapshot strategy involves creating periodic snapshots at different intervals. For example:

- **Hourly snapshots**: Keep the last 24
- **Daily snapshots**: Keep for 7-30 days
- **Weekly snapshots**: Keep for 1-3 months
- **Monthly snapshots**: Keep for 6-12 months
- **Yearly snapshots**: Keep for several years

You can automate snapshot creation using either the API or cron jobs:

### Method 1: Using the API (Recommended)

Add these entries to your crontab:
```cron
# Hourly snapshots
0 * * * * curl -X POST "http://localhost:8888/snapshot?periodicity=hourly"

# Daily snapshot at midnight
0 0 * * * curl -X POST "http://localhost:8888/snapshot?periodicity=daily"

# Weekly snapshot on Sundays
0 0 * * 0 curl -X POST "http://localhost:8888/snapshot?periodicity=weekly"

# Monthly snapshot on the 1st
0 0 1 * * curl -X POST "http://localhost:8888/snapshot?periodicity=monthly"

# Yearly snapshot on January 1st
0 0 1 1 * curl -X POST "http://localhost:8888/snapshot?periodicity=yearly"
```

### Method 2: Direct ZFS Commands

Create a snapshot script:
```bash
#!/bin/bash
# snapshot.sh - Create ZFS snapshots with proper naming

type=$1
if [ -z "$type" ]; then
  echo "Usage: $0 <type>"
  echo "Where <type> matches your policy (hourly, daily, etc.)"
  exit 1
fi

# Read from backupd config or set manually
pool="tank/data"  # Should match your local.root in backupd.toml
now=$(date +%Y%m%d-%H%M%S)
snapshot_name="$pool@$type-$now"

echo "Creating snapshot: $snapshot_name"
zfs snapshot -r "$snapshot_name"
```

Then add to crontab:
```cron
# Hourly snapshots
0 * * * * /path/to/snapshot.sh hourly

# Daily snapshot at midnight
0 0 * * * /path/to/snapshot.sh daily

# Weekly snapshot on Sundays
0 0 * * 0 /path/to/snapshot.sh weekly

# Monthly snapshot on the 1st
0 0 1 * * /path/to/snapshot.sh monthly

# Yearly snapshot on January 1st
0 0 1 1 * /path/to/snapshot.sh yearly
```

## Using backupd as a library

`backupd` is also a Go package. The daemon — sync loop, planner,
dashboard, control API — is `monks.co/backupd/daemon`, and the command
you install is a thin wrapper over it. A host program embeds it to
supply the things a deployment owns and a backup daemon should not
invent: a listener, an authorization layer, a log pipeline, and the
chrome of the site it is part of.

```go
d := daemon.New(daemon.Options{
	Config: conf,           // the only required field
	Logger: slog.Default(), // structured records go here
	Layout: myLayout,       // wrap each page in your own HTML
	OmitNav: true,          // ...and render its nav yourself
})

go d.Run(ctx)                     // the sync loop
http.Handle("/", d.Handler())     // the dashboard and control API
```

`Options{Config: conf}` alone gives you exactly what the command does;
every other field has a working default.

| API | Purpose |
|-----|---------|
| `daemon.New(Options) *Daemon` | Build a daemon. Starts nothing. |
| `(*Daemon).Run(ctx)` | The sync loop, until ctx is cancelled. |
| `(*Daemon).Handler()` | The dashboard and control API as an `http.Handler`. |
| `(*Daemon).Mount(prefix)` | The same, under a sub-path of your own mux. |
| `(*Daemon).Serve(ctx, addr)` | A listener of the daemon's own, for hosts that don't have one. |
| `(*Daemon).Go(ctx, addr)` | Run and Serve together — the standalone daemon. |
| `(*Daemon).View()` | The current derived state: verdict, issues, per-dataset health, cycle history. What the dashboard renders, for exporting to metrics or health checks. |
| `(*Daemon).Debug(ctx, dataset, w)` | Refresh one dataset and write what would happen to it. |
| `daemon.CallAPI(ctx, addr, method, path)` | Drive a running daemon's control API, as the subcommands do. |

### Logging

Every line the daemon writes goes to one place. By default that is the
standard `log` package; pass `Options.Logger` and it is your
`*slog.Logger` instead — including the in-memory ring buffers, whose
lines arrive labelled with the logger that wrote them
(`backupd.log=global`).

The records worth alerting on carry named attributes rather than
message text, so "are backups running" is a query:

| Attribute | Meaning |
|-----------|---------|
| `backupd.cycle.ok` | false on a cycle that failed, wholly or for some datasets. The record is at ERROR level too, so level-based routing catches it without knowing this vocabulary. |
| `backupd.cycle.paused`, `backupd.cycle.datasets`, `backupd.cycle.duration_ms`, `backupd.cycle.failures` | The rest of the cycle outcome. |
| `backupd.event` | A journal entry's level: info, warning, error. |
| `backupd.dataset` | The dataset a record concerns. |
| `backupd.snitch.ok` | false when a Dead Man's Snitch ping failed. |
| `backupd.log` | Which ring buffer a buffered line came from. |

### Page chrome

Without a layout, the daemon renders a complete HTML document. With
`Options.Layout`, it renders the page body and hands it to you:

```go
func myLayout(w http.ResponseWriter, r *http.Request, page daemon.Page) error {
	// page.Title names the page; page.Body is one <div class="backupd">
	// carrying its styles, scripts, and content.
	return mySite.Render(w, r, page.Title, page.Body)
}
```

`Layout` takes no framework-specific types, so anything that can write
HTML can be one. The body is safe to drop into a page whose stylesheet
the daemon knows nothing about: every rule it emits is scoped to
`.backupd`, its animation names are prefixed, and its colors are
`light-dark()` pairs that follow whatever `color-scheme` your page
declares. `daemon.Nav()` returns the daemon's own top-level links so you
can render them in your nav; `OmitNav` then stops it rendering them
again.

### Mount points

The dashboard can be served anywhere. Behind a reverse proxy that sets
`X-Forwarded-Prefix` and strips the path — the usual arrangement —
register `Handler()` and every link and fetch follows the prefix. Behind
your own mux, use `Mount("/backups")` instead.

### Privilege

`local.escalate` is a command prefix applied to every local zfs
command:

```toml
[local]
root = "tank/data"
escalate = ["sudo", "-n"]   # or ["doas"], or omitted to run as root
```

With it set, the daemon runs as an ordinary user and escalates per
command; the root check at startup relaxes accordingly, since a daemon
that escalates is not supposed to be root. The remote side takes no
prefix — it already arrives over ssh as whichever user the key
authenticates as.

Cancelling a transfer kills the whole process group, not just the
process the daemon started: `sudo` forks and waits whenever it allocates
a pty or logs I/O, so killing the wrapper alone can leave `zfs send`
running with the pipe still open. Where the daemon lacks the privilege
to signal what it escalated to — an unprivileged daemon cannot signal a
root process group — the kill is best-effort, and what stops the send is
the pipe closing behind it.

## Architecture

### Domain Model

The application uses a clear domain-driven design with the following core entities:

1. **Model**: The top-level system state containing all datasets and their current status
2. **Dataset**: Represents a ZFS dataset with its current snapshots, target state, metrics, and execution plan
3. **Snapshot**: Individual point-in-time backup with metadata (creation time, size, type)
4. **SnapshotInventory**: Tracks which snapshots exist at each location (local/remote)
5. **Operation**: Abstract representation of actions (transfers, deletions) to be performed
6. **Plan**: Ordered sequence of operations to transition from current to target state

### Operational Flow

**How backupd Works:**

1. **State Discovery and Assessment:**
   - Scans local ZFS datasets recursively from configured root
   - Connects to remote systems via SSH to catalog remote snapshots
   - Creates a complete inventory (`SnapshotInventory`) of all snapshots with metadata
   - Updates the global `Model` with current state for all datasets

2. **Goal State Calculation:**
   - Uses `CalculateTargetInventory` to determine ideal snapshot distribution
   - Applies retention policies (configurable per snapshot type)
   - Preserves critical snapshots:
     - Oldest snapshot on each location
     - Earliest and latest shared snapshots between locations
     - Policy-matching snapshots up to retention limits
   - Non-policy snapshots are candidates for deletion (with exceptions above)

3. **Plan Generation:**
   - `CalculateTransitionPlan` compares current vs target inventories
   - Creates `Operation` instances for each required action:
     - `SnapshotDeletion` / `SnapshotRangeDeletion` for removals
     - `InitialSnapshotTransfer` for first-time transfers
     - `SnapshotRangeTransfer` for incremental transfers
   - Groups adjacent operations into ranges for efficiency
   - `ValidatePlan` simulates execution to ensure correctness

4. **Execution:**
   - Processes each `PlanStep` sequentially
   - Tracks execution status (Pending → InProgress → Completed/Failed)
   - Uses ZFS send/receive with raw mode for transfers
   - Supports resumable transfers via ZFS receive tokens
   - Updates progress tracking for web UI visibility

5. **Monitoring and Reporting:**
   - Real-time status via web interface at http://localhost:8888
   - Progress tracking per dataset with operation logs
   - Dead Man's Snitch integration for external monitoring
   - Atomic state updates using thread-safe `Atom` wrapper

### System Components

**Core Packages:**
- `model/`: Domain entities and business logic
- `env/`: ZFS command execution and SSH communication, behind an
  `env.Interface` seam
- `sim/`: In-memory implementation of `env.Interface` — a simulated
  pair of pools with transfers, resume tokens, and fault injection —
  used by `-sim` mode and the end-to-end test suite
- `config/`: TOML configuration parsing and validation
- `view/`: The pure derivation layer between raw state and every UI
  claim — dataset health, staleness, typed issues, the system verdict,
  cycle progress. HTML and JSON both render from it, so they cannot
  disagree, and every judgment call is property-tested
- `status/`: Live activity tracking (sync phase, cycle queue, current
  step, transfer progress)
- `history/`: Cycle outcomes, per-dataset success/failure, executed ops
- `daemon/`: The daemon itself — sync loop, dashboard, control API —
  as an importable package, so a host program can supply its own
  listener, logging, authorization, and page chrome (see Using backupd
  as a library)
- `logger/`: Buffered in-memory operation logs, routed to the host's
  `*slog.Logger` when one is installed
- `atom/`: Thread-safe state management

**Concurrent Architecture:**
The service runs two main goroutines:
1. **Web Server**: Serves HTTP endpoints for UI and API
2. **Sync Loop**: Hourly execution of backup operations

**Key Design Patterns:**
- Immutable state with functional transformations
- Command pattern for operations
- Observer pattern for progress tracking
- Repository pattern for ZFS interactions

## License

See LICENSE file for details.