package model

import (
	"github.com/dustin/go-humanize"
)

// DatasetSize represents the physical storage size of a dataset
type DatasetSize struct {
	Used              int64 // Total on-disk space with children, including all snapshots
	LogicalReferenced int64 // Logical size of most recent snapshot (w/o children)
}

func (ds DatasetSize) String() string {
	return humanize.Bytes(uint64(ds.Used))
}

func (ds DatasetSize) HumanizedUsed() string {
	return humanize.Bytes(uint64(ds.Used))
}

func (ds DatasetSize) HumanizedLogical() string {
	return humanize.Bytes(uint64(ds.LogicalReferenced))
}

// StorageMetrics represents physical storage information for a dataset.
// This is observability data only - not used in planning or goal calculation.
type StorageMetrics struct {
	LocalSize  DatasetSize
	RemoteSize DatasetSize
	HasLocal   bool // Indicates if LocalSize is valid
	HasRemote  bool // Indicates if RemoteSize is valid
}

// NewStorageMetrics creates a new StorageMetrics with the given sizes
func NewStorageMetrics(localSize, remoteSize DatasetSize, hasLocal, hasRemote bool) StorageMetrics {
	return StorageMetrics{
		LocalSize:  localSize,
		RemoteSize: remoteSize,
		HasLocal:   hasLocal,
		HasRemote:  hasRemote,
	}
}

// LocalUsedString returns the humanized local used space or "-" if not available
func (sm StorageMetrics) LocalUsedString() string {
	if sm.HasLocal {
		return sm.LocalSize.HumanizedUsed()
	}
	return "-"
}

// LocalLogicalString returns the humanized local logical space or "-" if not available
func (sm StorageMetrics) LocalLogicalString() string {
	if sm.HasLocal {
		return sm.LocalSize.HumanizedLogical()
	}
	return "-"
}

// RemoteUsedString returns the humanized remote used space or "-" if not available
func (sm StorageMetrics) RemoteUsedString() string {
	if sm.HasRemote {
		return sm.RemoteSize.HumanizedUsed()
	}
	return "-"
}

// RemoteLogicalString returns the humanized remote logical space or "-" if not available
func (sm StorageMetrics) RemoteLogicalString() string {
	if sm.HasRemote {
		return sm.RemoteSize.HumanizedLogical()
	}
	return "-"
}
