package archive

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"kura/internal/catalog"
	"kura/internal/scan"
	"kura/internal/volume"
)

// RestoreResult is what one volume contributed to a restore.
type RestoreResult struct {
	Volume   string
	Restored int
	Bytes    int64

	// Failed lists objects this volume should have produced and could not.
	Failed []Finding

	// Elsewhere counts objects in the requested generation that live on other
	// volumes, so the operator is told what still has to be mounted rather than
	// left to guess from a tree that looks finished.
	Elsewhere map[string]int

	// Incomplete lists split files whose other fragments are on volumes not yet
	// restored. They are written as far as this volume can take them, and the
	// partial file is left with a suffix so nothing mistakes it for the whole.
	Incomplete []string

	// Lost lists objects the catalog already knows cannot be produced.
	Lost []string
}

// OK reports whether everything this volume owed was produced.
func (r *RestoreResult) OK() bool { return len(r.Failed) == 0 && len(r.Lost) == 0 }

// PartialSuffix marks a file that is only as complete as the volumes restored
// so far. A split file spans cartridges, and a half-written file under its real
// name is exactly the silent short answer that must never happen.
const PartialSuffix = ".kura-partial"

// Restore writes out the files the loaded volume holds, for the tree as it
// stood at gen.
//
// One volume per invocation, like everything else: a restore of a large
// generation is a sequence of mounts, and the result says what is still
// outstanding after each. It never quietly produces a directory that looks
// complete — a missing object is named, counted, and turns the exit non-zero
// (DESIGN §8.1d).
func Restore(ctx context.Context, vol volume.Volume, cat *catalog.Catalog, gen int, dest string, verify bool) (*RestoreResult, error) {
	// As in Verify: the loaded cartridge says which volume it is, and a restore
	// produces whatever that volume owes rather than being told in advance.
	h, err := ReadHeader(ctx, vol)
	if err != nil {
		return nil, err
	}
	res := &RestoreResult{Volume: h.Volume, Elsewhere: make(map[string]int)}

	var here []*catalog.Record
	for _, rec := range cat.At(gen) {
		switch {
		case rec.IsLost():
			res.Lost = append(res.Lost, rec.Path)
		case rec.UsesVolume(res.Volume):
			here = append(here, rec)
		default:
			for _, v := range rec.Volumes() {
				res.Elsewhere[v]++
			}
		}
	}

	byContainer := make(map[string][]*catalog.Record)
	for _, rec := range here {
		switch {
		case rec.Inside():
			byContainer[rec.Container] = append(byContainer[rec.Container], rec)
		case len(rec.Parts) > 0:
			res.restoreParts(ctx, vol, rec, dest)
		default:
			res.restoreObject(ctx, vol, rec, dest, verify)
		}
	}
	// Containers are read once each, in name order so a sequential medium is
	// read front to back rather than seeking between them.
	names := make([]string, 0, len(byContainer))
	for name := range byContainer {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		res.restoreContainer(ctx, vol, name, byContainer[name], dest, verify)
	}

	sort.Strings(res.Lost)
	sort.Strings(res.Incomplete)
	return res, nil
}

func (r *RestoreResult) restoreObject(ctx context.Context, vol volume.Volume, rec *catalog.Record, dest string, verify bool) {
	name := rec.ObjectName()
	rc, err := vol.Get(ctx, name)
	if err != nil {
		r.Failed = append(r.Failed, Finding{Path: rec.Path, Object: name, Err: err})
		return
	}
	defer rc.Close()

	n, err := writeFile(destPath(dest, rec.Path), rec, rc, verify)
	if err != nil {
		r.Failed = append(r.Failed, Finding{Path: rec.Path, Object: name, Err: err})
		return
	}
	r.Restored++
	r.Bytes += n
}

func (r *RestoreResult) restoreContainer(ctx context.Context, vol volume.Volume, name string, members []*catalog.Record, dest string, verify bool) {
	wanted := make(map[string]*catalog.Record, len(members))
	for _, m := range members {
		wanted[m.ObjectName()] = m
	}

	rc, err := vol.Get(ctx, name)
	if err != nil {
		for _, m := range members {
			r.Failed = append(r.Failed, Finding{Path: m.Path, Object: name, Err: err})
		}
		return
	}
	defer rc.Close()

	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			for _, m := range wanted {
				r.Failed = append(r.Failed, Finding{Path: m.Path, Object: name, Err: err})
			}
			return
		}
		rec, ok := wanted[hdr.Name]
		if !ok {
			continue
		}
		delete(wanted, hdr.Name)
		n, err := writeFile(destPath(dest, rec.Path), rec, tr, verify)
		if err != nil {
			r.Failed = append(r.Failed, Finding{Path: rec.Path, Object: name, Err: err})
			continue
		}
		r.Restored++
		r.Bytes += n
	}
	// Anything still wanted was not in the container the catalog said held it.
	for _, m := range wanted {
		r.Failed = append(r.Failed, Finding{
			Path: m.Path, Object: name,
			Err: fmt.Errorf("container does not hold member %q", m.ObjectName()),
		})
	}
}

// restoreParts writes the fragments of a split file that live on this volume,
// into the right offsets of a partial file.
func (r *RestoreResult) restoreParts(ctx context.Context, vol volume.Volume, rec *catalog.Record, dest string) {
	target := destPath(dest, rec.Path)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		r.Failed = append(r.Failed, Finding{Path: rec.Path, Err: err})
		return
	}
	partial := target + PartialSuffix

	f, err := os.OpenFile(partial, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		r.Failed = append(r.Failed, Finding{Path: rec.Path, Err: err})
		return
	}
	closed := false
	defer func() {
		if !closed {
			f.Close()
		}
	}()
	if err := f.Truncate(rec.Size); err != nil {
		r.Failed = append(r.Failed, Finding{Path: rec.Path, Err: err})
		return
	}

	var got int64
	for _, p := range rec.Parts {
		if p.Volume != r.Volume {
			r.Elsewhere[p.Volume]++
			continue
		}
		rc, err := vol.Get(ctx, p.Object)
		if err != nil {
			r.Failed = append(r.Failed, Finding{Path: rec.Path, Object: p.Object, Err: err})
			continue
		}
		h := scan.NewHasher(p.Length)
		n, err := io.Copy(io.NewOffsetWriter(f, p.Offset), io.TeeReader(rc, h))
		rc.Close()
		if err != nil {
			r.Failed = append(r.Failed, Finding{Path: rec.Path, Object: p.Object, Err: err})
			continue
		}
		if _, full, err := h.Sums(); err != nil || (p.Hash != "" && full != p.Hash) {
			r.Failed = append(r.Failed, Finding{
				Path: rec.Path, Object: p.Object,
				Err: errors.New("fragment did not read back as recorded"),
			})
			continue
		}
		got += n
	}

	r.Bytes += got

	// Completion cannot be judged from what this invocation wrote: the other
	// fragments were laid into the same file on earlier mounts, days ago. The
	// partial is checked against every part's digest instead, which settles
	// both "is it all here" and "is it all right" in one pass.
	if err := complete(f, rec); err != nil {
		r.Incomplete = append(r.Incomplete, rec.Path)
		return
	}
	if err := f.Close(); err != nil {
		r.Failed = append(r.Failed, Finding{Path: rec.Path, Err: err})
		return
	}
	closed = true
	// Every fragment is in and reads back as recorded. Only now does it get its
	// real name.
	if err := os.Rename(partial, target); err != nil {
		r.Failed = append(r.Failed, Finding{Path: rec.Path, Err: err})
		return
	}
	applyMeta(target, rec)
	r.Restored++
}

// complete reports whether every fragment of a split file is present in the
// partial and matches the digest recorded for it.
func complete(f *os.File, rec *catalog.Record) error {
	var covered int64
	for _, p := range rec.Parts {
		if p.Hash == "" {
			return fmt.Errorf("fragment at %d has no recorded digest", p.Offset)
		}
		h := scan.NewHasher(p.Length)
		if _, err := io.Copy(h, io.NewSectionReader(f, p.Offset, p.Length)); err != nil {
			return err
		}
		_, full, err := h.Sums()
		if err != nil {
			return err
		}
		if full != p.Hash {
			return fmt.Errorf("fragment at %d is not yet in place", p.Offset)
		}
		covered += p.Length
	}
	if covered != rec.Size {
		return fmt.Errorf("fragments cover %d of %d bytes", covered, rec.Size)
	}
	return nil
}

// writeFile materialises one restored file, checking it against its recorded
// digest on the way past.
//
// It writes to a temporary name and renames, so an interrupted restore never
// leaves a short file under the real name for someone to trust.
func writeFile(target string, rec *catalog.Record, src io.Reader, verify bool) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".kura-restore-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())

	var w io.Writer = tmp
	h := scan.NewHasher(rec.Size)
	if verify {
		w = io.MultiWriter(tmp, h)
	}
	n, err := io.Copy(w, src)
	if err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if n != rec.Size {
		return 0, fmt.Errorf("read %d bytes, the catalog says %d", n, rec.Size)
	}
	if verify {
		_, full, err := h.Sums()
		if err != nil {
			return 0, err
		}
		if rec.FullHash != "" && full != rec.FullHash {
			return 0, errors.New("digest mismatch: the restored bytes are not what was written")
		}
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return 0, err
	}
	applyMeta(target, rec)
	return n, nil
}

// applyMeta restores what the catalog remembers about a file. Failures here are
// deliberately not fatal: wrong permissions on a recovered file are a nuisance,
// missing content is a disaster, and conflating them helps nobody.
func applyMeta(target string, rec *catalog.Record) {
	if rec.Mode != 0 {
		os.Chmod(target, os.FileMode(rec.Mode).Perm())
	}
	if rec.ModTime != 0 {
		t := time.Unix(rec.ModTime, 0)
		os.Chtimes(target, t, t)
	}
}

// destPath maps a catalog path into the restore directory. The archive stores
// absolute paths, so they are rebased rather than followed.
func destPath(dest, p string) string {
	return filepath.Join(dest, filepath.FromSlash(strings.TrimLeft(p, "/")))
}
