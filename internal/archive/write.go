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

	// Plan is the plan this volume belongs to. It is carried only so that a
	// checkpoint can be bound to one cycle; the plan and catalog are already
	// checked against each other before Write is called (DESIGN §10.2).
	Plan string

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

	// Pause reports whether the run has been asked to stop. It is polled
	// between objects and never inside one: an object name is a pure function
	// of its path, Put refuses to overwrite, and the medium never reclaims, so
	// an object abandoned half-written would burn its own name. Finishing what
	// is in flight costs latency — one object, which on the real archive can be
	// a 10 GiB file — and buys a medium on which every object is whole.
	// DESIGN §11.1.
	Pause func() bool

	// Resume is the checkpoint a previously paused run left, or nil. Objects it
	// vouches for are taken on trust instead of being read back; everything
	// else on the medium still goes the slow way.
	Resume *Checkpoint

	// VerifyResumed re-reads the objects adopted from Resume and checks them
	// against the digests the pause recorded. Off by default, because the
	// mandatory verify pass at the end of the volume is the same check done
	// once rather than twice (DESIGN §11.1).
	VerifyResumed bool

	// OnStart is called before each object is placed, and OnObject after. The
	// pair exists because a pause has to be able to say what it is waiting for:
	// the stop lands at the next boundary, which for a large file is minutes
	// away, and a run that went silent instead would look hung.
	OnStart func(name string, size int64)

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

	// Adopted counts objects taken from a pause's checkpoint without being read
	// back at all. Kept apart from Resumed because the difference between them
	// is the difference between a deliberate pause and a crash, and the
	// operator is owed that distinction.
	Adopted int

	// Paused is set when the run stopped on request rather than finishing the
	// volume. Nothing was committed to the catalog in that case; Checkpoint is
	// what has to be persisted instead, and it is nil for a dry run, which
	// placed nothing to vouch for.
	Paused     bool
	Checkpoint *Checkpoint

	// Remaining is how many of this volume's plan entries were still to go.
	Remaining int

	// ResumeNotes is what a resume could not take on trust, and why. Worth
	// printing: each line is an object that will be re-read or a file the next
	// cycle owes a write.
	ResumeNotes []string

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
	// The plan names the volume this is meant to be, and Reconcile hands back
	// the header that settles what it actually is: stamped from pv.ID when the
	// cartridge was blank, read off the medium when it was not. Every record
	// below takes its volume ID from there rather than from the drive, which
	// knows nothing about which cartridge the operator pushed in.
	h, err := Reconcile(ctx, vol, opt.Archive, pv.ID, opt.Generation, opt.DryRun)
	if err != nil {
		return nil, err
	}

	present, err := existing(ctx, vol)
	if err != nil {
		return nil, err
	}

	w := &writer{
		vol:       vol,
		id:        h.Volume,
		opt:       opt,
		present:   present,
		known:     make(map[string]bool),
		problemAt: make(map[string]int),
		res:       &Result{Volume: h.Volume},
	}

	// A checkpoint is trusted only for the cycle and the cartridge it names. One
	// about another volume is not stale, it is about something else — a pause on
	// volume 3 has to survive the operator choosing to write volume 5 — so it is
	// reported and left untouched rather than consumed.
	if opt.Resume.Describes(opt.Archive, opt.Plan, opt.Generation, h.Volume) {
		// The notes for these come from refuse(), when an entry actually meets
		// one. Emitting them here as well said everything twice, and it named
		// objects that no entry on this volume was going to ask about.
		w.adopted, w.refused = adoptObjects(opt.Resume, h.Volume, present)
		// The files the paused run could not read are not on the medium and are
		// not in the plan's gift to retry here. Carrying them forward is what
		// stops a pause from quietly swallowing the one list that tells the
		// operator the next cycle owes those files a write.
		for _, pr := range opt.Resume.Problems {
			w.problemAt[pr.Path] = len(w.res.Problems)
			w.res.Problems = append(w.res.Problems, Problem{Path: pr.Path, Err: errors.New(pr.Err)})
			w.known[pr.Path] = true
		}
	} else if opt.Resume != nil {
		w.res.ResumeNotes = []string{fmt.Sprintf(
			"the paused write in this workdir is volume %s of generation %d, not %s of %d; leaving it alone",
			opt.Resume.Volume, opt.Resume.Generation, h.Volume, opt.Generation)}
	}

	// Entries is the authoritative list: the grouped Items view does not carry
	// its files through the plan file, and reading both would write everything
	// twice. Containers are collected first so each tar is built in one pass.
	containers := make(map[string][]plan.Entry)
	var order []string
	paused := false
	for _, e := range pv.Entries {
		if w.asked() {
			paused = true
			break
		}
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
		if paused || w.asked() {
			paused = true
			break
		}
		if err := w.container(ctx, name, containers[name]); err != nil {
			return nil, err
		}
	}

	if paused {
		// Sync first, checkpoint second, and never the other way round. Put
		// promises atomicity and Sync promises durability, and this pause exists
		// so the host can be switched off — so a checkpoint written ahead of the
		// flush would vouch for bytes still sitting in the page cache.
		// DESIGN §11.1.
		w.res.Paused = true
		w.res.Remaining = len(pv.Entries) - w.done
		if !opt.DryRun {
			if err := vol.Sync(ctx); err != nil {
				return nil, fmt.Errorf("syncing %s: %w", w.id, err)
			}
			w.res.Checkpoint = w.checkpoint(w.res.Remaining)
		}
		// Deliberately no catalog, on the medium or locally. A mid-volume copy
		// on the medium would be the one the cartridge keeps, since writeCatalog
		// skips a name already present — leaving an index describing only what
		// preceded the pause. And committing the records locally would make
		// alreadyWritten skip the very cartridge the next run is meant to
		// finish. The records are in the checkpoint instead.
		return w.res, nil
	}

	// The records go into the catalog before it is written to the volume, so
	// the copy the medium carries describes the volume it is on.
	//
	// The generation goes in for the same reason. Setting it in the caller
	// after Write returned left every on-volume copy claiming generation 0 —
	// invisible while nothing reads those copies back, and wrong on the one day
	// something has to.
	cat.Add(w.res.Records...)
	cat.Header.Generation = max(cat.Header.Generation, opt.Generation)
	info := cat.Header.Volume(w.id)
	if info.Written == "" {
		info.Written = time.Now().UTC().Format(time.RFC3339)
	}
	info.Objects += int64(w.res.Objects + w.res.Containers + w.carriedObjects)
	info.Bytes += w.res.Bytes + w.carriedBytes

	if !opt.DryRun {
		if err := w.writeCatalog(ctx, cat); err != nil {
			return nil, err
		}
		// Put leaves objects atomically visible but not necessarily on the
		// platter. This is the point the volume becomes safe to eject.
		if err := vol.Sync(ctx); err != nil {
			return nil, fmt.Errorf("syncing %s: %w", w.id, err)
		}
	}
	return w.res, nil
}

type writer struct {
	vol volume.Volume

	// id is what the medium says it is, from the header Reconcile returned.
	// Held here rather than asked of vol because a volume has no identity to
	// give: see the note on volume.Volume.
	id string

	opt     Options
	present map[string]int64 // objects already on the volume, by name
	res     *Result

	// adopted is what a pause vouched for, by object name. An entry here is
	// taken without reading the medium; everything else already on it is read
	// back, which is all a crash leaves the writer able to do. The whole Placed
	// is kept rather than just its records, so a run that pauses again hands the
	// next one the same source stamps rather than dropping its inheritance.
	adopted map[string]Placed

	// refused is what a pause placed and this run will not vouch for, by object
	// name, with the reason. Kept apart from adopted because the two need
	// opposite handling and neither is "write it": the name on the medium is
	// taken either way.
	refused map[string]string

	// problemAt indexes Result.Problems by path, so a file that failed before a
	// pause and fails again after it is named once rather than accumulating an
	// entry per resume. The freshest error wins: it describes the attempt the
	// operator is reading about.
	problemAt map[string]int

	// known is the paths a pause already reported as unreadable, so a container
	// it placed without them is recognised as complete rather than as missing a
	// member the plan asked for.
	known map[string]bool

	// carried counts what was already on the medium and is being vouched for
	// rather than written: adopted from a pause, or read back after a crash.
	//
	// The inventory needs it and the throughput figure must not have it. Without
	// it a resumed volume reports only what the last run happened to write — a
	// cartridge finished from a pause that vouched for all but one object listed
	// as holding that one object, which is most of a terabyte of under-reporting
	// on real media.
	carriedObjects int
	carriedBytes   int64

	// placed is what went onto the medium this run, in order, for the
	// checkpoint. done counts the plan entries accounted for, however — written,
	// adopted, read back, or reported — which is what makes "how much is left"
	// answerable at a pause.
	placed []Placed
	done   int
}

// asked reports whether a stop has been requested. Polled between objects only.
func (w *writer) asked() bool { return w.opt.Pause != nil && w.opt.Pause() }

// resumeVerdict is what a checkpoint has to say about one object name.
type resumeVerdict int

const (
	// resumeUnknown: the pause said nothing about this object, so it is written,
	// or read back if a crash left it on the medium.
	resumeUnknown resumeVerdict = iota
	// resumeAdopt: take its records on trust.
	resumeAdopt
	// resumeRefuse: it is on the medium and cannot be vouched for.
	resumeRefuse
)

// consult asks the checkpoint about one object name.
//
// files is the plan's members for a container and nil otherwise. A container the
// pause placed without a member the plan asks for is refused rather than adopted
// short: either the pause reported that file as unreadable, in which case it is
// already carried forward, or the two disagree about what is on the medium and
// neither is entitled to guess.
//
// With VerifyResumed the object is read back and checked against the digests the
// pause recorded before its records are accepted.
func (w *writer) consult(ctx context.Context, name string, files []plan.Entry) (Placed, resumeVerdict, string) {
	if why, refused := w.refused[name]; refused {
		return Placed{}, resumeRefuse, why
	}
	pl, ok := w.adopted[name]
	if !ok {
		return Placed{}, resumeUnknown, ""
	}
	if missing := uncovered(pl, files, w.known); missing != "" {
		return Placed{}, resumeRefuse, fmt.Sprintf("the pause did not record %s, which the plan puts in it", missing)
	}
	if w.opt.VerifyResumed {
		if err := w.confirmAdopted(ctx, name, pl.Records); err != nil {
			return Placed{}, resumeRefuse, err.Error()
		}
	}
	w.res.Adopted++
	return pl, resumeAdopt, ""
}

// refuse reports an object a pause placed that this run will not vouch for.
//
// Deliberately not a fallback to reading the medium. The name is taken and Put
// will not overwrite it, so no corrected copy can go here; and reading the object
// back would only compare the medium against itself, which is how a damaged
// object comes to be recorded against a digest of its own damage. Reporting it
// leaves the file out of the catalog and the next cycle writes it to another
// cartridge, which is what resume-trust.nix asks for.
func (w *writer) refuse(name, why string, paths ...string) {
	w.res.ResumeNotes = append(w.res.ResumeNotes, fmt.Sprintf("%s: %s", name, why))
	for _, path := range paths {
		w.problem(path, fmt.Errorf("a pause placed %s and it can no longer be vouched for (%s); "+
			"the name is taken on %s, so the next cycle writes this file to another volume", name, why, w.id))
	}
}

// uncovered names a planned file that an adopted object does not account for, or
// "" when it accounts for all of them.
func uncovered(pl Placed, files []plan.Entry, known map[string]bool) string {
	if len(files) == 0 {
		return ""
	}
	have := make(map[string]bool, len(pl.Records))
	for _, rec := range pl.Records {
		have[rec.Path] = true
	}
	for _, e := range files {
		if !have[e.Path] && !known[e.Path] {
			return e.Path
		}
	}
	return ""
}

// kept takes ownership of what one object contributed: its records go to the
// catalog, and the object itself is remembered in case this run is paused and
// has to hand it to the next one.
//
// Nil sources mean the object cannot be vouched for — nothing was able to stat
// what it was built from — so it is recorded but left out of any checkpoint.
// The next resume reads it back instead, which is the safe direction.
// carry is kept for an object that was already on the medium. It is the
// inventory's only chance to learn those bytes: nothing incremented the volume's
// counters when the run that placed them died or was paused.
func (w *writer) carry(name string, stored int64, recs []*catalog.Record, sources []Source) {
	w.carriedObjects++
	w.carriedBytes += stored
	w.kept(name, stored, recs, sources)
}

func (w *writer) kept(name string, stored int64, recs []*catalog.Record, sources []Source) {
	if sources != nil {
		w.placed = append(w.placed, Placed{Object: name, Stored: stored, Records: recs, Sources: sources})
	}
	w.res.Records = append(w.res.Records, recs...)
}

// starting announces an object before it is placed, so a pause can name what it
// is waiting for.
func (w *writer) starting(name string, size int64) {
	if w.opt.OnStart != nil {
		w.opt.OnStart(name, size)
	}
}

// confirmAdopted checks an adopted object against the digests the pause wrote
// down. It is the verify pass brought forward, and the flag that turns it on
// exists for an operator who would rather pay twice than once.
func (w *writer) confirmAdopted(ctx context.Context, name string, recs []*catalog.Record) error {
	switch {
	case len(recs) == 1 && len(recs[0].Parts) == 1:
		part := recs[0].Parts[0]
		_, full, err := digestObject(ctx, w.vol, name, part.Length)
		if err != nil {
			return err
		}
		if part.Hash != "" && full != part.Hash {
			return errors.New("the fragment on the medium is not what the pause recorded")
		}
		return nil
	case len(recs) == 1 && recs[0].Container == "":
		_, full, err := digestObject(ctx, w.vol, name, recs[0].Size)
		if err != nil {
			return err
		}
		if recs[0].FullHash != "" && full != recs[0].FullHash {
			return errors.New("the object on the medium is not what the pause recorded")
		}
		return nil
	default:
		digests, err := digestContainer(ctx, w.vol, name)
		if err != nil {
			return err
		}
		for _, rec := range recs {
			d, ok := digests[rec.Object]
			if !ok {
				return fmt.Errorf("does not hold %s", rec.Path)
			}
			if rec.FullHash != "" && d.full != rec.FullHash {
				return fmt.Errorf("%s is not what the pause recorded", rec.Path)
			}
		}
		return nil
	}
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
	w.done++

	switch pl, verdict, why := w.consult(ctx, name, nil); verdict {
	case resumeAdopt:
		w.carry(name, pl.Stored, pl.Records, pl.Sources)
		return nil
	case resumeRefuse:
		w.refuse(name, why, e.Path)
		return nil
	}

	if _, done := w.present[name]; done {
		rec, src, err := w.resumeObject(ctx, e, name)
		if err != nil {
			return err
		}
		if rec != nil {
			w.carry(name, w.present[name], []*catalog.Record{rec}, []Source{src})
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

	w.starting(name, e.Size)
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
	w.kept(name, e.Size,
		[]*catalog.Record{w.record(e, info, name, "", sampled, full)},
		[]Source{sourceOf(e.ReadFrom(), info)})
	return nil
}

// resumeObject rebuilds the record for an object an interrupted run already
// placed, by reading it back. That read doubles as a check on the part of the
// volume written before the interruption.
//
// This is the crash path, and it is the expensive one: every object already on
// the medium is read again before the write can go on. A pause leaves a
// checkpoint precisely so it does not come through here (DESIGN §11.1).
func (w *writer) resumeObject(ctx context.Context, e plan.Entry, name string) (*catalog.Record, Source, error) {
	if w.present[name] != e.Size {
		return nil, Source{}, fmt.Errorf("%s on %s is %d bytes, the plan says %d: refusing to guess which is right",
			name, w.id, w.present[name], e.Size)
	}
	sampled, full, err := digestObject(ctx, w.vol, name, e.Size)
	if err != nil {
		w.problem(e.Path, fmt.Errorf("re-reading a resumed object: %w", err))
		return nil, Source{}, nil
	}
	info, err := os.Stat(e.ReadFrom())
	if err != nil {
		w.problem(e.Path, err)
		return nil, Source{}, nil
	}
	return w.record(e, info, name, "", sampled, full), sourceOf(e.ReadFrom(), info), nil
}

// container builds and places one tar.
//
// It is staged in a local file first because Put must be told the exact size up
// front and a tar's encoded length is not knowable until it has been written —
// header padding and long-name records depend on the names themselves.
func (w *writer) container(ctx context.Context, name string, files []plan.Entry) error {
	w.done += len(files)

	switch pl, verdict, why := w.consult(ctx, name, files); verdict {
	case resumeAdopt:
		w.carry(name, pl.Stored, pl.Records, pl.Sources)
		return nil
	case resumeRefuse:
		w.refuse(name, why, paths(files)...)
		return nil
	}
	if _, done := w.present[name]; done {
		return w.resumeContainer(ctx, name, files)
	}

	// Announced before the staging rather than after it: building the tar is most
	// of the work, so a pause asked for during it would otherwise be told it was
	// waiting on whichever object came before. The size is the members' own
	// bytes, which is an estimate — the encoded length is not knowable until the
	// tar exists — and an estimate is what the acknowledgement needs.
	w.starting(name, sourceBytes(files))

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
	recs := make([]*catalog.Record, 0, len(members))
	sources := make([]Source, 0, len(members))
	for _, m := range members {
		recs = append(recs, w.record(m.entry, m.info, memberName(m.entry.Path), name, m.sampled, m.full))
		sources = append(sources, sourceOf(m.entry.ReadFrom(), m.info))
	}
	w.kept(name, size, recs, sources)
	return nil
}

// resumeContainer rebuilds records for a container an interrupted run already
// placed, by reading its members back.
func (w *writer) resumeContainer(ctx context.Context, name string, files []plan.Entry) error {
	digests, err := digestContainer(ctx, w.vol, name)
	if err != nil {
		return fmt.Errorf("re-reading resumed container %s: %w", name, err)
	}
	var recs []*catalog.Record
	sources := make([]Source, 0, len(files))
	vouch := true
	for _, e := range files {
		d, ok := digests[memberName(e.Path)]
		if !ok {
			if w.known[e.Path] {
				// A pause already reported this file as unreadable, so the
				// container is complete without it rather than missing it.
				continue
			}
			// The name is taken by a container that holds something else. Put
			// refuses to overwrite, and guessing which is right is exactly the
			// mistake that loses data.
			return fmt.Errorf("container %s on %s does not hold %s, which the plan says it should",
				name, w.id, e.Path)
		}
		info, err := os.Stat(e.ReadFrom())
		if err != nil {
			w.problem(e.Path, err)
			// One member whose source has gone means nothing can say whether
			// the rest of this container still matches the tree, so a later
			// pause must not vouch for it.
			vouch = false
			continue
		}
		recs = append(recs, w.record(e, info, memberName(e.Path), name, d.sampled, d.full))
		sources = append(sources, sourceOf(e.ReadFrom(), info))
	}
	if !vouch {
		sources = nil
	}
	w.carry(name, w.present[name], recs, sources)
	w.res.Resumed++
	return nil
}

// fragment writes one piece of a file that was split across volumes.
//
// The record it produces carries only this volume's part. Merging the parts of
// one path happens in the catalog, as the other volumes are written.
func (w *writer) fragment(ctx context.Context, e plan.Entry) error {
	name := fragmentObjectName(e.Path, e.Part)
	w.done++

	switch pl, verdict, why := w.consult(ctx, name, nil); verdict {
	case resumeAdopt:
		w.carry(name, pl.Stored, pl.Records, pl.Sources)
		return nil
	case resumeRefuse:
		w.refuse(name, why, e.Path)
		return nil
	}

	rec := &catalog.Record{
		Path:    e.Path,
		Size:    e.FileSize,
		ModTime: e.ModTime,
		Gen:     w.opt.Generation,
	}
	part := catalog.Part{Volume: w.id, Object: name, Offset: e.Offset, Length: e.Length}

	if _, done := w.present[name]; done {
		_, full, err := digestObject(ctx, w.vol, name, e.Length)
		if err != nil {
			return fmt.Errorf("re-reading resumed fragment %s: %w", name, err)
		}
		part.Hash = full
		rec.Parts = []catalog.Part{part}
		// A fragment whose source cannot be stat'd is still recorded, as it
		// always was — the bytes on the medium are the bytes the plan asked
		// for. It just cannot be vouched for later, so nil sources keep it out
		// of any checkpoint this run goes on to write.
		var sources []Source
		if info, err := os.Stat(e.ReadFrom()); err == nil {
			sources = []Source{sourceOf(e.ReadFrom(), info)}
		}
		w.carry(name, w.present[name], []*catalog.Record{rec}, sources)
		w.res.Resumed++
		return nil
	}

	f, err := os.Open(e.ReadFrom())
	if err != nil {
		w.problem(e.Path, err)
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		w.problem(e.Path, err)
		return nil
	}
	if _, err := f.Seek(e.Offset, io.SeekStart); err != nil {
		w.problem(e.Path, err)
		return nil
	}
	w.starting(name, e.Length)
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
	w.kept(name, e.Length, []*catalog.Record{rec}, []Source{sourceOf(e.ReadFrom(), info)})
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
		Volume:    w.id,
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
		if _, err := io.Copy(io.Discard, r); err != nil {
			return err
		}
	} else if err := w.vol.Put(ctx, name, size, r); err != nil {
		return err
	}
	if w.opt.OnObject != nil {
		w.opt.OnObject(name, size)
	}
	return nil
}

func (w *writer) problem(path string, err error) {
	if at, seen := w.problemAt[path]; seen {
		w.res.Problems[at].Err = err
	} else {
		w.problemAt[path] = len(w.res.Problems)
		w.res.Problems = append(w.res.Problems, Problem{Path: path, Err: err})
	}
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
		name := catalogObjectName(w.opt.Generation, i)
		if _, done := w.present[name]; done {
			// Only reachable when this same generation was interrupted after
			// placing a copy. Its content is already complete — writeCatalog
			// runs after every object — so skipping is right. A *later*
			// generation cannot land here at all now that the name carries the
			// generation, which is what used to leave a volume describing only
			// the first cycle that touched it.
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

// sourceBytes is what a set of entries holds before tar framing.
func sourceBytes(files []plan.Entry) int64 {
	var n int64
	for _, e := range files {
		n += e.Size
	}
	return n
}

// paths is the source files a set of plan entries covers.
func paths(files []plan.Entry) []string {
	out := make([]string, len(files))
	for i, e := range files {
		out[i] = e.Path
	}
	return out
}
