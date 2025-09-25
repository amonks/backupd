# backupd

A daemon for managing ZFS snapshot backups with local and remote targets.

## Overview

`backupd` is a service that manages ZFS snapshots across local and remote systems. It provides automated snapshot creation, replication, and retention policy enforcement to ensure your data is safely backed up according to configurable policies.

Key features:
- Automated ZFS snapshot replication with intelligent planning
- Type-based retention policies with configurable limits
- Real-time web dashboard for monitoring and control
- Resumable transfers with progress tracking
- RESTful API for snapshot creation
- Dead Man's Snitch integration for external monitoring
- Dry-run mode for safe testing
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

2. **Snapshot Retention Management:**
   - Applies per-type retention policies (e.g., keep 24 hourly, 7 daily)
   - Processes snapshots by creation time, keeping newest first
   - Preserves critical snapshots:
     - Oldest snapshot at each location (historical baseline)
     - Earliest shared snapshot (incremental transfer base)
     - Latest shared snapshot (synchronization point)
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

- Must be run as root (for ZFS operations)
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
- `/etc/backupd.toml`
- `/usr/local/etc/backupd.toml`
- `/opt/local/etc/backupd.toml`
- `/Library/Application Support/co.monks.backupd/backupd.toml`

### Configuration Structure

```toml
# Optional: External monitoring via Dead Man's Snitch
# Get your snitch ID from https://deadmanssnitch.com
snitch_id = "your-snitch-id"

[local]
# Root dataset to backup (all child datasets included)
root = "tank/data"

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
- `-addr <address>`: Server address for the web interface (default: "0.0.0.0:8888")
- `-dryrun`: Refresh state but don't execute transfers or deletions (preview mode)

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
curl -X POST "http://localhost:8888/snapshot?periodicity=daily"
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

The web interface provides real-time monitoring:
- **Global view**: http://localhost:8888/global - Overview of all datasets
- **Root dataset**: http://localhost:8888/root - Root dataset status
- **Specific dataset**: http://localhost:8888/dataset-name - Individual dataset details
- **Automatic redirect**: http://localhost:8888/ → /global

### REST API Endpoints

#### Create Snapshot
```
POST /snapshot?periodicity=<type>
```
Creates a new recursive snapshot for the configured local root dataset.

**Parameters:**
- `periodicity`: Snapshot type (e.g., "hourly", "daily", "weekly")

**Example:**
```bash
curl -X POST "http://localhost:8888/snapshot?periodicity=hourly"
```

**Response:**
- 200 OK: Snapshot created successfully
- 400 Bad Request: Missing periodicity parameter
- 500 Internal Server Error: Creation failed

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
- `env/`: ZFS command execution and SSH communication
- `config/`: TOML configuration parsing and validation
- `sync/`: Synchronization status tracking
- `progress/`: Operation progress logging
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