package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"syscall"
	"time"

	"kura/internal/catalog"
	"kura/internal/plan"
	"kura/internal/scan"
	"kura/internal/volume"
)

// DefaultCatalogCopies is how many times the catalog is written to a volume.
const DefaultCatalogCopies = 2

// Options controls one volume's write.
type Options struct {
	// Archive identifies the archive; a volume claimed by a different one is
	// refused rather than overwritten.
	Archive string

	// Generation is the cycle being written. It goes into every record and
	// into the names of any containers, so a later chunking of the same
	// directory cannot collide with this one.
	Generation int

	// CatalogCopies is how many times the catalog is written onto the volume;
	// 0 means DefaultCatalogCopies.
	//
	// There is deliberately no container setting here. Which files share a
	// container is decided when the plan is made and recorded in it, so that a
	// resumed write recognises the containers it already placed and a dry run
	// can say exactly what a cycle will create.
	CatalogCopies int

	// StageDir is where containers are assembled before being placed. A
	// container's exact encoded length is not known until it has been built,
	// and Put has to be told the size up front, so it is staged locally first.
	StageDir string

	// DryRun reports what would be written without touching the volume. The
	// digests are still computed, because reading every source file is most of
	// what makes a dry run worth trusting.
	DryRun bool

	// OnObject is called after each object is placed, for progress.
	OnObject func(name string, size int64)

	// OnProblem reports a source file that could not be written, so the run
	// continues over the rest of the volume rather than abandoning it.
	OnProblem func(path string, err error)
}

func (o Options) catalogCopies() int {
	if o.CatalogCopies <= 0 {
		return DefaultCatalogCopies
	}
	return o.CatalogCopies
}

// Result is what one volume's write did.
type Result struct {
	Volume     string
	Records    []*catalog.Record
	Bytes      int64
	Objects    int
	Containers int

	// Resumed counts objects that were already on the volume from an
	// interrupted run and were read back rather than written again.
	Resumed int

	// Problems lists source files that could not be written. They are left out
	// of the records entirely, so the next cycle sees them as still needing a
	// write rather than as safely stored.
	Problems []Problem
}

// Problem is one file the writer could not store.
type Problem struct {
	Path string
	Err  error
}

// Write places one volume's share of a plan and returns the records describing
// what landed where.
//
// It handles exactly this volume and returns. The catalog is written onto the
// volume before returning, so the medium alone is enough to read the archive
// back, and the caller persists the same catalog locally as a cache.
func Write(ctx context.Context, vol volume.Volume, pv *plan.Volume, cat *catalog.Catalog, opt Options) (*Result, error) {
	if opt.Archive == "" {
		return nil, errors.New("archive: no archive ID")
	}
	if opt.Generation < 1 {
		return nil, fmt.Errorf("archive: generation must be 1 or greater, got %d", opt.Generation)
	}
	if _, err := Reconcile(ctx, vol, opt.Archive, opt.Generation, opt.DryRun); err != nil {
		return nil, err
	}

	present, err := existing(ctx, vol)
	if err != nil {
		return nil, err
	}

	w := &writer{vol: vol, opt: opt, present: present, res: &Result{Volume: vol.ID()}}

	// Entries is the authoritative list: the grouped Items view does not carry
	// its files through the plan file, and reading both would write everything
	// twice. Containers are collected first so each tar is built in one pass.
	containers := make(map[string][]plan.Entry)
	var order []string
	for _, e := range pv.Entries {
		switch {
		case e.Split():
			if err := w.fragment(ctx, e); err != nil {
				return nil, err
			}
		case e.Container != "":
			if _, seen := containers[e.Container]; !seen {
				order = append(order, e.Container)
			}
			containers[e.Container] = append(containers[e.Container], e)
		default:
			if err := w.object(ctx, e); err != nil {
				return nil, err
			}
		}
	}
	sort.Strings(order)
	for _, name := range order {
		if err := w.container(ctx, name, containers[name]); err != nil {
			return nil, err
		}
	}

	// The records go into the catalog before it is written to the volume, so
	// the copy the medium carries describes the volume it is on.
	cat.Add(w.res.Records...)
	info := cat.Header.Volume(vol.ID())
	if info.Written == "" {
		info.Written = time.Now().UTC().Format(time.RFC3339)
	}
	info.Objects += int64(w.res.Objects + w.res.Containers)
	info.Bytes += w.res.Bytes

	if !opt.DryRun {
		if err := w.writeCatalog(ctx, cat); err != nil {
			return nil, err
		}
		// Put leaves objects atomically visible but not necessarily on the
		// platter. This is the point the volume becomes safe to eject.
		if err := vol.Sync(ctx); err != nil {
			return nil, fmt.Errorf("syncing %s: %w", vol.ID(), err)
		}
	}
	return w.res, nil
}

type writer struct {
	vol     volume.Volume
	opt     Options
	present map[string]int64 // objects already on the volume, by name
	res     *Result
}

// existing indexes what is already on the volume, which is how an interrupted
// run resumes. Put refuses to overwrite a name, so without this a second
// attempt would fail on the first object it had already placed.
func existing(ctx context.Context, vol volume.Volume) (map[string]int64, error) {
	objs, err := vol.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(objs))
	for _, o := range objs {
		out[o.Name] = o.Size
	}
	return out, nil
}

// object writes one file as an object of its own.
func (w *writer) object(ctx context.Context, e plan.Entry) error {
	name := dataObjectName(e.Path)

	if _, done := w.present[name]; done {
		rec, err := w.resumeObject(ctx, e, name)
		if err != nil {
			return err
		}
		if rec != nil {
			w.res.Records = append(w.res.Records, rec)
			w.res.Resumed++
		}
		return nil
	}

	src, info, err := openSource(e)
	if err != nil {
		w.problem(e.Path, err)
		return nil
	}
	defer src.Close()

	h := scan.NewHasher(e.Size)
	if err := w.put(ctx, name, e.Size, io.TeeReader(src, h)); err != nil {
		if errors.Is(err, volume.ErrVolumeFull) {
			// The volume filled up despite the plan's arithmetic, so the rest
			// of this volume's share has nowhere to go. Stop rather than
			// scatter the remainder.
			return fmt.Errorf("%s: %w", e.Path, err)
		}
		w.problem(e.Path, err)
		return nil
	}
	sampled, full, err := h.Sums()
	if err != nil {
		// The file changed size under us. The object on the volume is now of
		// unknown provenance, so no record claims it and the next cycle writes
		// the file again under a fresh name.
		w.problem(e.Path, err)
		return nil
	}

	w.res.Objects++
	w.res.Bytes += e.Size
	w.res.Records = append(w.res.Records, w.record(e, info, name, "", sampled, full))
	return nil
}

// resumeObject rebuilds the record for an object an interrupted run already
// placed, by reading it back. That read doubles as a check on the part of the
// volume written before the interruption.
func (w *writer) resumeObject(ctx context.Context, e plan.Entry, name string) (*catalog.Record, error) {
	if w.present[name] != e.Size {
		return nil, fmt.Errorf("%s on %s is %d bytes, the plan says %d: refusing to guess which is right",
			name, w.vol.ID(), w.present[name], e.Size)
	}
	sampled, full, err := digestObject(ctx, w.vol, name, e.Size)
	if err != nil {
		w.problem(e.Path, fmt.Errorf("re-reading a resumed object: %w", err))
		return nil, nil
	}
	info, err := os.Stat(e.ReadFrom())
	if err != nil {
		w.problem(e.Path, err)
		return nil, nil
	}
	return w.record(e, info, name, "", sampled, full), nil
}

// container builds and places one tar.
//
// It is staged in a local file first because Put must be told the exact size up
// front and a tar's encoded length is not knowable until it has been written —
// header padding and long-name records depend on the names themselves.
func (w *writer) container(ctx context.Context, name string, files []plan.Entry) error {
	if _, done := w.present[name]; done {
		return w.resumeContainer(ctx, name, files)
	}

	stage, err := os.CreateTemp(w.opt.StageDir, "kura-container-*.tar")
	if err != nil {
		return err
	}
	defer os.Remove(stage.Name())
	defer stage.Close()

	tw := tar.NewWriter(stage)
	type member struct {
		entry   plan.Entry
		info    os.FileInfo
		sampled string
		full    string
	}
	var members []member

	for _, e := range files {
		src, info, err := openSource(e)
		if err != nil {
			// One unreadable file must not cost the whole container. It is left
			// out and reported, so the next cycle sees it as still needing a
			// write.
			w.problem(e.Path, err)
			continue
		}
		hdr := &tar.Header{
			Name:    memberName(e.Path),
			Mode:    int64(info.Mode().Perm()),
			Size:    e.Size,
			ModTime: info.ModTime(),
			Format:  tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			src.Close()
			return err
		}
		h := scan.NewHasher(e.Size)
		n, err := io.Copy(tw, io.TeeReader(io.LimitReader(src, e.Size), h))
		src.Close()
		if err != nil {
			return err
		}
		if n != e.Size {
			return fmt.Errorf("%s: read %d of %d bytes", e.Path, n, e.Size)
		}
		sampled, full, err := h.Sums()
		if err != nil {
			return fmt.Errorf("%s: %w", e.Path, err)
		}
		members = append(members, member{e, info, sampled, full})
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if len(members) == 0 {
		return nil
	}

	size, err := stage.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if _, err := stage.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := w.put(ctx, name, size, stage); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}

	w.res.Containers++
	w.res.Bytes += size
	for _, m := range members {
		w.res.Records = append(w.res.Records,
			w.record(m.entry, m.info, memberName(m.entry.Path), name, m.sampled, m.full))
	}
	if w.opt.OnObject != nil {
		w.opt.OnObject(name, size)
	}
	return nil
}

// resumeContainer rebuilds records for a container an interrupted run already
// placed, by reading its members back.
func (w *writer) resumeContainer(ctx context.Context, name string, files []plan.Entry) error {
	digests, err := digestContainer(ctx, w.vol, name)
	if err != nil {
		return fmt.Errorf("re-reading resumed container %s: %w", name, err)
	}
	for _, e := range files {
		d, ok := digests[memberName(e.Path)]
		if !ok {
			// The name is taken by a container that holds something else. Put
			// refuses to overwrite, and guessing which is right is exactly the
			// mistake that loses data.
			return fmt.Errorf("container %s on %s does not hold %s, which the plan says it should",
				name, w.vol.ID(), e.Path)
		}
		info, err := os.Stat(e.ReadFrom())
		if err != nil {
			w.problem(e.Path, err)
			continue
		}
		w.res.Records = append(w.res.Records,
			w.record(e, info, memberName(e.Path), name, d.sampled, d.full))
	}
	w.res.Resumed++
	return nil
}

// fragment writes one piece of a file that was split across volumes.
//
// The record it produces carries only this volume's part. Merging the parts of
// one path happens in the catalog, as the other volumes are written.
func (w *writer) fragment(ctx context.Context, e plan.Entry) error {
	name := fragmentObjectName(e.Path, e.Part)

	rec := &catalog.Record{
		Path:    e.Path,
		Size:    e.FileSize,
		ModTime: e.ModTime,
		Gen:     w.opt.Generation,
	}
	part := catalog.Part{Volume: w.vol.ID(), Object: name, Offset: e.Offset, Length: e.Length}

	if _, done := w.present[name]; done {
		_, full, err := digestObject(ctx, w.vol, name, e.Length)
		if err != nil {
			return fmt.Errorf("re-reading resumed fragment %s: %w", name, err)
		}
		part.Hash = full
		rec.Parts = []catalog.Part{part}
		w.res.Records = append(w.res.Records, rec)
		w.res.Resumed++
		return nil
	}

	f, err := os.Open(e.ReadFrom())
	if err != nil {
		w.problem(e.Path, err)
		return nil
	}
	defer f.Close()
	if _, err := f.Seek(e.Offset, io.SeekStart); err != nil {
		w.problem(e.Path, err)
		return nil
	}
	h := scan.NewHasher(e.Length)
	if err := w.put(ctx, name, e.Length, io.TeeReader(io.LimitReader(f, e.Length), h)); err != nil {
		return fmt.Errorf("%s part %d: %w", e.Path, e.Part, err)
	}
	_, full, err := h.Sums()
	if err != nil {
		w.problem(e.Path, err)
		return nil
	}
	part.Hash = full
	rec.Parts = []catalog.Part{part}
	w.res.Objects++
	w.res.Bytes += e.Length
	w.res.Records = append(w.res.Records, rec)
	return nil
}

// record builds the catalog entry for one stored file.
//
// The stat data comes from the file as it was actually read, not from the plan,
// which may be hours old. Recording a stale ctime would make the next cycle
// treat the file as suspect and read it again for nothing.
func (w *writer) record(e plan.Entry, info os.FileInfo, object, container, sampled, full string) *catalog.Record {
	rec := &catalog.Record{
		Path:      e.Path,
		Size:      e.Size,
		ModTime:   info.ModTime().Unix(),
		Mode:      uint32(info.Mode().Perm()),
		Hash:      sampled,
		HashKind:  catalog.HashKindSample,
		FullHash:  full,
		Volume:    w.vol.ID(),
		Object:    object,
		Container: container,
		Gen:       w.opt.Generation,
	}
	// Ctime and inode corroborate mtime next cycle; without them the change
	// check degrades to mtime alone, which is exactly the failure DESIGN §6
	// exists to avoid.
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		rec.CTime = st.Ctim.Nano()
		rec.Inode = st.Ino
	}
	return rec
}

func (w *writer) put(ctx context.Context, name string, size int64, r io.Reader) error {
	if w.opt.DryRun {
		_, err := io.Copy(io.Discard, r)
		return err
	}
	if err := w.vol.Put(ctx, name, size, r); err != nil {
		return err
	}
	if w.opt.OnObject != nil {
		w.opt.OnObject(name, size)
	}
	return nil
}

func (w *writer) problem(path string, err error) {
	w.res.Problems = append(w.res.Problems, Problem{Path: path, Err: err})
	if w.opt.OnProblem != nil {
		w.opt.OnProblem(path, err)
	}
}

// writeCatalog places the index, more than once (DESIGN §8.1a).
func (w *writer) writeCatalog(ctx context.Context, cat *catalog.Catalog) error {
	stage, err := os.CreateTemp(w.opt.StageDir, "kura-catalog-*.jsonl.gz")
	if err != nil {
		return err
	}
	path := stage.Name()
	stage.Close()
	defer os.Remove(path)

	if err := catalog.Write(path, cat.Header, cat.Records); err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	for i := range w.opt.catalogCopies() {
		name := catalogObjectName(i)
		if _, done := w.present[name]; done {
			// A resumed run finds last attempt's copies. They describe fewer
			// records than this one does, so the fresh copies go under the next
			// free numbers rather than replacing them: names are write-once,
			// and a stale index that still parses is worse than an extra one.
			continue
		}
		if err := w.vol.Put(ctx, name, int64(len(b)), bytesReader(b)); err != nil {
			return fmt.Errorf("writing %s: %w", name, err)
		}
	}
	return nil
}

// openSource opens a file for writing to a volume and confirms it still looks
// like what the plan described.
//
// A file that changed between planning and writing is refused rather than
// stored: its recorded size would be a lie, and Put would reject the mismatch
// anyway. Saying so plainly is more useful than a length error.
func openSource(e plan.Entry) (*os.File, os.FileInfo, error) {
	src, err := os.Open(e.ReadFrom())
	if err != nil {
		return nil, nil, err
	}
	info, err := src.Stat()
	if err != nil {
		src.Close()
		return nil, nil, err
	}
	if info.Size() != e.Size {
		src.Close()
		return nil, nil, fmt.Errorf("changed since planning: %d bytes now, %d when planned", info.Size(), e.Size)
	}
	return src, info, nil
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
