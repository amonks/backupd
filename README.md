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

Configuration is loaded from one of the following locations (in order):
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

## Web Interface

The web interface is available at:
- Global view: http://localhost:8888/global
- Root dataset: http://localhost:8888/root
- Specific dataset: http://localhost:8888/<dataset-path>

## Architecture

`backupd` operates by:
1. Scanning local and remote ZFS datasets
2. Comparing current state with desired state based on policies
3. Creating a plan of operations (transfers, deletions)
4. Executing the plan to achieve the desired state
5. Monitoring and resuming incomplete transfers
6. Providing a web interface to view status

The service runs in two concurrent goroutines:
- Web server for the UI
- Sync process that runs hourly to maintain backups

## License

See LICENSE file for details.