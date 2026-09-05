// Package filevol stores a volume as a directory on an ordinary filesystem.
//
// It exists first, before any tape support, because it is the only backend
// whose failure paths can be produced on demand. Corruption, a volume filling
// mid-run, a write interrupted halfway, a catalog that has diverged from its
// medium — each of those has to be testable thousands of times in seconds, and
// none of them can be arranged against a tape drive. The tape backend is then
// written against an interface that already has a working implementation.
//
// It is also a real backend. An archive kept on a shelf of rotating hard disks
// is the most common offline-backup arrangement there is, and this is it.
package filevol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"kura/internal/volume"
)

// tmpDir holds partial writes. It lives inside the volume so that the rename
// completing a Put never crosses a filesystem boundary, and is excluded from
// everything the volume reports.
const tmpDir = ".kura-tmp"

// Backend is a drive that a directory can be loaded into.
//
// Load stands in for inserting media. Keeping that as an explicit step, rather
// than pointing the backend at a fixed path, is what lets a test drive the
// swap workflow the operator actually performs.
type Backend struct {
	dir      string // currently loaded volume, empty when nothing is in
	capacity int64  // declared; 0 means ask the filesystem
	health   volume.Health
	open     *Volume
}

// New returns an empty drive. A non-zero capacity declares a fixed volume size,
// which is what makes "the volume filled up" reachable in a test without
// writing terabytes; zero defers to the underlying filesystem's free space.
func New(capacity int64) *Backend {
	return &Backend{
		capacity: capacity,
		health:   volume.Health{Status: volume.StatusUnknown},
	}
}

// Name identifies the backend.
func (b *Backend) Name() string { return "file" }

// Load inserts the volume rooted at dir, creating it if necessary.
func (b *Backend) Load(dir string) error {
	if b.open != nil {
		return errors.New("filevol: a volume is still open; close it first")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b.dir = dir
	return nil
}

// SetHealth overrides what Health reports. A real drive answers from SMART or
// TapeAlert; this backend has nothing to answer from, so tests inject the
// verdict they want to exercise.
func (b *Backend) SetHealth(h volume.Health) { b.health = h }

// Open mounts the loaded volume.
func (b *Backend) Open(ctx context.Context) (volume.Volume, error) {
	if b.dir == "" {
		return nil, volume.ErrNoVolume
	}
	if b.open != nil {
		return b.open, nil
	}
	used, err := usedBytes(b.dir)
	if err != nil {
		return nil, err
	}
	v := &Volume{
		backend: b,
		dir:     b.dir,
		used:    used,
	}
	if err := os.MkdirAll(filepath.Join(v.dir, tmpDir), 0o755); err != nil {
		return nil, err
	}
	b.open = v
	return v, nil
}

// Release ejects. Safe to call with nothing loaded.
func (b *Backend) Release(ctx context.Context) error {
	if b.open != nil {
		if err := b.open.Close(); err != nil {
			return err
		}
	}
	b.dir = ""
	return nil
}

// Health reports the injected verdict.
func (b *Backend) Health(ctx context.Context) (volume.Health, error) {
	return b.health, nil
}

// Volume is a directory holding one volume's objects.
//
// It has no identity of its own, deliberately: the directory is a location, the
// way a drive is, and which volume is in it is settled by reading the header
// the medium carries. See the note on volume.Volume.
type Volume struct {
	backend *Backend
	dir     string
	used    int64
	closed  bool
}

// Capacity reports total and remaining bytes.
func (v *Volume) Capacity() (total, free int64, err error) {
	if v.closed {
		return 0, 0, os.ErrClosed
	}
	if v.backend.capacity > 0 {
		return v.backend.capacity, max(v.backend.capacity-v.used, 0), nil
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(v.dir, &st); err != nil {
		return 0, 0, err
	}
	total = int64(st.Blocks) * int64(st.Bsize)
	free = int64(st.Bavail) * int64(st.Bsize)
	return total, free, nil
}

// Put writes an object atomically: into the temp directory, flushed, then
// renamed into place. A crash therefore leaves either the whole object or
// nothing, never a truncated file that a later run would trust.
//
// An existing name is an error rather than an overwrite. On media that never
// reclaims space, writing the same name twice means the catalog has lost track
// of what it already stored, and failing loudly beats silently shadowing it.
func (v *Volume) Put(ctx context.Context, name string, size int64, r io.Reader) error {
	if v.closed {
		return os.ErrClosed
	}
	if size < 0 {
		return fmt.Errorf("filevol: negative size for %q", name)
	}
	dst, err := v.resolve(name)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("filevol: %q already exists on the volume at %s", name, v.dir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	_, free, err := v.Capacity()
	if err != nil {
		return err
	}
	if size > free {
		return fmt.Errorf("%w: %s needs %d bytes, %d free", volume.ErrVolumeFull, name, size, free)
	}

	tmp, err := os.CreateTemp(filepath.Join(v.dir, tmpDir), "put-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has happened

	written, err := io.Copy(tmp, io.LimitReader(r, size))
	if err != nil {
		tmp.Close()
		return err
	}
	// A short or over-long reader means the caller's size is wrong, which
	// almost always means its catalog entry is too. Refuse rather than store
	// something whose recorded length is a lie.
	if written != size {
		tmp.Close()
		return fmt.Errorf("filevol: %q declared %d bytes, reader yielded %d", name, size, written)
	}
	var extra [1]byte
	if n, _ := r.Read(extra[:]); n > 0 {
		tmp.Close()
		return fmt.Errorf("filevol: %q declared %d bytes, reader yielded more", name, size)
	}
	// Deliberately no fsync here. Put promises atomicity, which the rename
	// below provides; Sync promises durability. Flushing every object instead
	// would cost a disk round trip per file — at the 1.66 M objects a real
	// archive reaches, over an hour of nothing but waiting — to guard against a
	// crash whose damage the mandatory verify pass would catch anyway.
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}
	v.used += size
	return nil
}

// Get opens an object.
func (v *Volume) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	if v.closed {
		return nil, os.ErrClosed
	}
	p, err := v.resolve(name)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", volume.ErrNotFound, name)
	}
	return f, err
}

// Stat reports one object's metadata.
func (v *Volume) Stat(ctx context.Context, name string) (volume.Object, error) {
	if v.closed {
		return volume.Object{}, os.ErrClosed
	}
	p, err := v.resolve(name)
	if err != nil {
		return volume.Object{}, err
	}
	fi, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return volume.Object{}, fmt.Errorf("%w: %s", volume.ErrNotFound, name)
	}
	if err != nil {
		return volume.Object{}, err
	}
	return volume.Object{Name: name, Size: fi.Size(), ModTime: fi.ModTime()}, nil
}

// List enumerates every object, in name order.
func (v *Volume) List(ctx context.Context) ([]volume.Object, error) {
	if v.closed {
		return nil, os.ErrClosed
	}
	var out []volume.Object
	err := filepath.WalkDir(v.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(v.dir, p)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			if rel == tmpDir {
				return fs.SkipDir
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, volume.Object{
			Name:    filepath.ToSlash(rel),
			Size:    fi.Size(),
			ModTime: fi.ModTime(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Sync makes everything written durable. Put leaves objects atomically visible
// but not necessarily on the platter; this is what a caller must reach before
// treating a volume as finished.
func (v *Volume) Sync(ctx context.Context) error {
	if v.closed {
		return os.ErrClosed
	}
	// Flush the objects themselves, then the directories that name them —
	// a durable file nothing links to is no better than no file at all.
	var dirs []string
	err := filepath.WalkDir(v.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if filepath.Base(p) == tmpDir {
				return fs.SkipDir
			}
			dirs = append(dirs, p)
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		return f.Sync()
	})
	if err != nil {
		return err
	}
	for _, p := range dirs {
		d, err := os.Open(p)
		if err != nil {
			return err
		}
		if err := d.Sync(); err != nil {
			d.Close()
			return err
		}
		d.Close()
	}
	return nil
}

// Close unmounts, discarding any partial writes left in the temp directory.
func (v *Volume) Close() error {
	if v.closed {
		return nil
	}
	v.closed = true
	if v.backend != nil {
		v.backend.open = nil
	}
	return os.RemoveAll(filepath.Join(v.dir, tmpDir))
}

// resolve maps an object name to a path inside the volume, refusing anything
// that would escape it. Names reach here from a catalog that may have been read
// off a medium someone else wrote, so they are not trusted input.
func (v *Volume) resolve(name string) (string, error) {
	if name == "" {
		return "", errors.New("filevol: empty object name")
	}
	if path.IsAbs(name) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("filevol: object name %q must be relative", name)
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("filevol: object name %q escapes the volume", name)
	}
	if clean == tmpDir || strings.HasPrefix(clean, tmpDir+"/") {
		return "", fmt.Errorf("filevol: object name %q is reserved", name)
	}
	return filepath.Join(v.dir, filepath.FromSlash(clean)), nil
}

// usedBytes totals the objects already present, ignoring the temp directory.
func usedBytes(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			if filepath.Base(p) == tmpDir {
				return fs.SkipDir
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		total += fi.Size()
		return nil
	})
	return total, err
}

var _ volume.Backend = (*Backend)(nil)
var _ volume.Volume = (*Volume)(nil)
