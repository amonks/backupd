# backupd

A daemon for managing ZFS snapshot backups with local and remote targets.

## Overview

`backupd` is a service that manages ZFS snapshots across local and remote systems. It provides automated snapshot creation, replication, and retention policy enforcement to ensure your data is safely backed up according to configurable policies.

Key features:
- Local and remote ZFS snapshot management
- Configurable retention policies for different snapshot types
- Web interface for monitoring backup status
- Resumable snapshot transfers
- Dead Man's Snitch monitoring integration

### Functional Description

**What backupd Does:**

1. **Snapshot Retention Management:**
   - Enforces retention policies for local snapshots based on snapshot type and age
   - Deletes snapshots that exceed retention limits or don't match policy types
   - Preserves critical snapshots required for system function (oldest, shared)

2. **Remote Backup Management:**
   - Transfers policy-matching snapshots to remote storage systems
   - Uses ZFS's efficient incremental transfer capabilities
   - Ensures remote systems maintain policy-compliant snapshot sets

3. **Policy-Based Data Lifecycle:**
   - Snapshots are categorized by type (hourly, daily, weekly, etc.)
   - Each type has configurable retention counts
   - Newer snapshots are preserved over older ones within each type

4. **Snapshot Pruning Rules:**
   - Snapshots without matching policy types are deleted (with limited exceptions)
   - When count limits are exceeded, oldest snapshots of that type are removed first
   - Special snapshots are preserved regardless of policy (oldest, shared between local/remote)

5. **Transfer Logic:**
   - Only transfers snapshots that match remote policy configuration
   - Requires at least one common snapshot for incremental transfers
   - Can't transfer snapshots that are older than existing remote snapshots

## Requirements

- Must be run as root
- ZFS filesystem
- FreeBSD/Linux

## Installation

Build from source:

```
go build -o backupd
```

## Configuration

Configuration is loaded from one of the following locations (in order of precedence):
- `/etc/backupd.toml`
- `/usr/local/etc/backupd.toml`
- `/opt/local/etc/backupd.toml`
- `/Library/Application Support/co.monks.backupd/backupd.toml`

Example configuration:

```toml
# Optional Dead Man's Snitch ID for monitoring
snitch_id = "your-snitch-id"

[remote]
ssh_key = "/path/to/ssh/key"
ssh_host = "backup-server.example.com"
root = "tank/backups"

# Retention policy for remote snapshots
# Format: snapshot-type = count
[remote.policy]
hourly = 24
daily = 7
weekly = 4
monthly = 6
yearly = 2

[local]
root = "tank/data"

# Retention policy for local snapshots
[local.policy]
hourly = 24
daily = 7
weekly = 4
monthly = 12
yearly = 5
```

## Usage

### Command Line Arguments

- `-debug <dataset>`: Debug a specific dataset (performs refresh and plan but no transfers)
- `-logfile <path>`: Log to a file instead of stdout
- `-addr <address>`: Server address for the web interface (default: "0.0.0.0:8888")
- `-dryrun`: Refresh state but don't transfer or delete snapshots

### Basic Usage

```
# Run as a service
sudo backupd

# Debug a specific dataset
sudo backupd -debug pool/dataset

# Run with a specific log file
sudo backupd -logfile /var/log/backupd.log
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

## Web Interface

The web interface is available at:
- Global view: http://localhost:8888/global
- Root dataset: http://localhost:8888/root
- Specific dataset: http://localhost:8888/<dataset-path>

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

You can automate snapshot creation using cron jobs. Here's a simple example script that creates a snapshot with the appropriate naming convention:

```bash
#!/bin/bash
# Usage: ./snapshot.sh daily|weekly|monthly|yearly

type=$1
if [ -z "$type" ]; then
  echo "Usage: ./snapshot.sh daily|weekly|monthly|yearly"
  exit 1
fi

pool="tank/data"  # Your dataset name
now=$(date +%Y-%m-%d-%H:%M:%S)
snapshot_name="$pool@$type-$now"

echo "Creating snapshot $snapshot_name"
zfs snapshot -r "$snapshot_name"
```

Add this to your crontab to run at appropriate intervals:
```
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

### Operational Flow

**How backupd Works:**

1. **State Discovery and Assessment:**
   - Scans local ZFS datasets using system commands
   - Connects to remote systems via SSH to catalog remote snapshots
   - Creates a complete inventory of all snapshots with metadata (creation time, type)

2. **Goal State Calculation:**
   - Constructs an ideal state based on policy configuration
   - Determines which snapshots should exist on local and remote systems
   - Incorporates special preservation rules for critical snapshots
   - Ignores snapshots with types not found in policy (with limited exceptions)

3. **Plan Generation:**
   - Compares current state with goal state to identify differences
   - Creates a series of operations (transfers, deletions) to reach goal state
   - Groups adjacent snapshots into ranges for efficient operations when possible
   - Validates plan against expected outcomes to catch errors

4. **Execution:**
   - Deletes snapshots on both local and remote systems that aren't in goal state
   - Transfers snapshots using ZFS send/receive for efficient incremental backups
   - Manages snapshot dependencies to ensure transfers can be completed
   - Tracks progress and can resume interrupted transfers

5. **Monitoring and Reporting:**
   - Provides real-time status via web interface
   - Logs all operations for auditing
   - Sends notifications through Dead Man's Snitch if configured
   - Presents results in human-readable format

### System Components

The service runs in two concurrent goroutines:
- Web server for the UI (provides status monitoring)
- Sync process that runs hourly to maintain backups (executes core backup logic)

Core system components involve:
- Dataset and snapshot management (model package)
- Plan generation and execution (operations)
- Remote system communication (SSH)
- Web status reporting

## License

See LICENSE file for details.