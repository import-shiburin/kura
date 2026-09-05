// Package volume is the seam between the archive and the media it lives on.
//
// Everything above this package — planning, packing, the catalog, generations,
// encryption, verification — is medium-agnostic. Everything a particular medium
// needs to be told is behind these two interfaces. The split between Backend
// and Volume mirrors the physical arrangement: a device persists and volumes
// come and go through it, one at a time, by hand.
package volume

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	// ErrNoVolume means the device has nothing loaded.
	ErrNoVolume = errors.New("no volume loaded")
	// ErrNotFound means the named object is not on this volume.
	ErrNotFound = errors.New("object not found")
	// ErrVolumeFull means the object will not fit in the space remaining.
	ErrVolumeFull = errors.New("volume full")
	// ErrReadOnly means the volume cannot be written to.
	ErrReadOnly = errors.New("volume is read-only")
)

// Backend is a device that presents one volume at a time: a tape drive, an
// optical drive, a dock for rotating disks, or a directory standing in for any
// of them.
type Backend interface {
	// Name identifies the backend in logs and errors.
	Name() string

	// Open mounts whatever volume is currently loaded. It returns ErrNoVolume
	// when the device is empty, which is an ordinary condition rather than a
	// failure — the operator has simply not inserted anything yet.
	Open(ctx context.Context) (Volume, error)

	// Release unmounts and ejects, so the operator can swap media. It must be
	// safe to call when nothing is loaded.
	Release(ctx context.Context) error

	// Health reports what the device knows about itself and the loaded medium.
	//
	// This cannot be answered through the filesystem. Every correction happens
	// below us and is invisible: the drive's ECC either succeeds, in which case
	// the read simply works, or it fails, in which case we get EIO. A backend
	// that only holds a mount point can never return anything but
	// StatusUnknown.
	//
	// Answering it properly needs the device itself alongside the mount — SCSI
	// LOG SENSE and MAM for tape, SMART for a disk — which generally means raw
	// device access and elevated privileges. Backends that have it should
	// return real numbers; those that do not must say StatusUnknown rather than
	// guess. See DESIGN.md §8.1 for the two signals that are observable without
	// any of that.
	Health(ctx context.Context) (Health, error)
}

// Volume is one mounted unit of media.
//
// Objects are named, and names may contain slashes: an implementation is free
// to lay them out as a directory tree or to flatten them, but must return from
// List exactly the names it was given to Put.
//
// There is deliberately no ID method. A volume's identity is written on the
// volume, in kura.volume.json, and archive.VolumeIDOn is what reads it. A
// backend cannot answer the question: a drive is a fixed mount point and the
// cartridge in it is whichever one the operator pushed in, so anything the
// backend could offer — a directory name, a device path — describes the drive
// rather than the medium. This was got wrong once. The file backend returned
// its directory's base name, which made the wrong-cartridge check compare the
// medium's header against a string chosen by whoever ran mount: it passed only
// when the operator had already named the mount point after the cartridge they
// believed was loaded, so it confirmed that belief instead of testing it.
type Volume interface {
	// Capacity reports the total and remaining bytes. Free is what Put can
	// still accept, so an implementation that reserves space for an index
	// subtracts that here rather than surprising the caller later.
	Capacity() (total, free int64, err error)

	// Put writes an object. The reader must yield exactly size bytes; a
	// mismatch is an error and leaves no object behind. Put is atomic: after a
	// failure or a crash, the object either exists whole or not at all, so a
	// resumed run never mistakes a partial write for a finished one.
	Put(ctx context.Context, name string, size int64, r io.Reader) error

	// Get opens an object for reading.
	Get(ctx context.Context, name string) (io.ReadCloser, error)

	// Stat reports one object's metadata.
	Stat(ctx context.Context, name string) (Object, error)

	// List enumerates every object on the volume.
	List(ctx context.Context) ([]Object, error)

	// Sync flushes whatever index the medium keeps, so that what has been
	// written survives a power loss.
	Sync(ctx context.Context) error

	// Close releases the mount without ejecting.
	Close() error
}

// Object is one stored item.
type Object struct {
	Name    string
	Size    int64
	ModTime time.Time
}

// Status is a backend-independent verdict on a device and its medium.
type Status int

const (
	// StatusUnknown means the backend cannot report health, which is the
	// honest answer for most optical drives and is not itself a problem.
	StatusUnknown Status = iota

	StatusHealthy

	// StatusDegrading means errors are still being corrected but their rate is
	// rising.
	//
	// It exists because this archive has no redundancy: once a read fails, the
	// medium's own error correction has already been exhausted and the data is
	// gone. The only defence is to notice while the volume is still fully
	// readable and copy it to fresh media — which is what this status is for.
	// It is a migration trigger, not a repair signal, and it is worth acting on
	// months before anything becomes unreadable.
	StatusDegrading

	// StatusNeedsAttention covers the device rather than the medium: cleaning
	// overdue, a failing drive, errors across every volume rather than one.
	StatusNeedsAttention
)

func (s Status) String() string {
	switch s {
	case StatusHealthy:
		return "healthy"
	case StatusDegrading:
		return "degrading"
	case StatusNeedsAttention:
		return "needs attention"
	default:
		return "unknown"
	}
}

// Health is what a backend can say about its device and the loaded medium.
//
// Only Status is meaningful to the core; the rest is for the operator to read.
// Detail is deliberately untyped because the interesting fields differ wildly —
// TapeAlert flags, SMART attributes, disc quality scans — and inventing a union
// of them would serve nobody.
type Health struct {
	Status           Status
	CleaningRequired bool
	Detail           map[string]string
}
