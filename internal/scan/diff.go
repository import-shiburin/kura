package scan

import (
	"path/filepath"
	"sort"
	"sync"

	"kura/internal/catalog"
)

// Reuse pairs a file on disk with a catalog record whose content it matches, so
// the cycle can record a new name for bytes that are already on media instead
// of writing them again.
//
// It covers moves, copies, and content that was deleted generations ago and has
// come back: because space is never reclaimed, anything ever written is still
// readable on its original volume.
type Reuse struct {
	From *catalog.Record
	To   *File
}

// Delta is the classification of a scan against the catalog.
type Delta struct {
	New      []*File
	Modified []*File
	Reused   []Reuse
	Deleted  []*catalog.Record
	Metadata []*File // symlinks, hardlinks, specials: catalog-only, no data

	// Refreshed holds files whose content was verified identical but whose
	// recorded ctime or inode is stale. Nothing needs writing to media; the
	// catalog entry just needs its metadata updated so the next cycle settles
	// them on stat data alone.
	Refreshed []*File

	UnchangedFiles int64
	UnchangedBytes int64
	NewBytes       int64
	ModifiedBytes  int64
	ReusedBytes    int64
	DeletedBytes   int64

	// HashedFiles counts how many files had to be opened to resolve a match.
	HashedFiles int64
	HashedBytes int64

	// ConfirmedFull counts matches verified against a whole-file digest rather
	// than accepted on the sampled one alone.
	ConfirmedFull int64

	// ReplacingLost counts files rewritten because the copy the catalog points
	// at is gone, not because anything on disk changed. It is reported apart
	// from the ordinary modified count because it measures damage rather than
	// activity: a non-zero figure means media is failing or history was
	// reclaimed, and those bytes are being re-protected from the source.
	ReplacingLost int64

	// Suspect counts files whose size and mtime matched but whose ctime or
	// inode did not, and CaughtByCTime how many of those turned out to have
	// genuinely changed. A non-zero CaughtByCTime is a file an mtime-only
	// check would have silently skipped forever.
	Suspect       int64
	CaughtByCTime int64
	Unverifiable  int64
}

// Options controls how a scan is compared against the catalog.
type Options struct {
	Match MatchMode
	Jobs  int

	// IgnoreCTime and IgnoreInode relax the corroborating checks, named after
	// the equivalent restic flags. Needed on filesystems without stable inodes,
	// and after a sweeping metadata change that would otherwise make every file
	// suspect.
	IgnoreCTime bool
	IgnoreInode bool
}

// ToWrite returns the files whose data actually has to reach media.
func (d *Delta) ToWrite() []*File {
	out := make([]*File, 0, len(d.New)+len(d.Modified))
	out = append(out, d.New...)
	out = append(out, d.Modified...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// WriteBytes is the total payload of ToWrite.
func (d *Delta) WriteBytes() int64 { return d.NewBytes + d.ModifiedBytes }

// MatchMode controls how far Diff goes to prove two files hold the same bytes.
type MatchMode int

const (
	// MatchOff settles everything from stat data; nothing is opened.
	MatchOff MatchMode = iota
	// MatchSample accepts the sampled digest as proof.
	MatchSample
	// MatchFull re-reads a candidate in full before accepting a match, whenever
	// the catalog record carries a whole-file digest to compare against.
	MatchFull
)

// Diff classifies a scan against the catalog.
//
// Matching is deliberately bounded. A file is only opened when its exact size
// collides with some record that carries a hash; everything else is settled
// with stat data alone, so a routine cycle reads almost nothing.
func Diff(files []*File, cat *catalog.Catalog, opt Options, onErr func(string, error)) *Delta {
	d := new(Delta)
	mode := opt.Match
	onDisk := make(map[string]bool, len(files))

	var candidates []*File
	var suspects []suspect
	for _, f := range files {
		onDisk[f.Path] = true

		if !f.IsRegular() {
			d.Metadata = append(d.Metadata, f)
			continue
		}

		rec, known := cat.Lookup(f.Path)
		if !known {
			candidates = append(candidates, f)
			continue
		}
		// A lost version has to be written again whatever the stat data says:
		// the bytes the catalog points at are gone, and the file is still on
		// the source. Marking something lost therefore re-protects it on the
		// next cycle for free, which is the self-healing property in DESIGN
		// §5.1 — it needs no machinery beyond this branch.
		if rec.IsLost() {
			d.ReplacingLost++
			d.Modified = append(d.Modified, f)
			d.ModifiedBytes += f.Size
			continue
		}
		switch classify(rec, f, opt) {
		case stateUnchanged:
			d.UnchangedFiles++
			d.UnchangedBytes += f.Size
		case stateSuspect:
			suspects = append(suspects, suspect{file: f, rec: rec})
		default:
			d.Modified = append(d.Modified, f)
			d.ModifiedBytes += f.Size
		}
	}
	d.resolveSuspects(suspects, opt, onErr)

	// Only live records can newly disappear. Running this over the whole
	// history would re-report every past deletion on every cycle.
	for _, rec := range cat.LiveRecords() {
		if !onDisk[rec.Path] {
			d.Deleted = append(d.Deleted, rec)
			d.DeletedBytes += rec.Size
		}
	}

	if mode == MatchOff || len(candidates) == 0 {
		d.finishNew(candidates)
		sortFiles(d.Modified)
		return d
	}

	// Index every record whose bytes are on media by size. Tombstoned records
	// are included on purpose: their data was never reclaimed, so re-added
	// content costs nothing to record.
	//
	// Lost records are excluded, and that exclusion is load-bearing. Pointing a
	// new path at bytes that are known to be gone would turn one damaged object
	// into two unrestorable paths, and would do it silently, since a reuse
	// writes nothing and so is never verified.
	bySize := make(map[int64][]*catalog.Record)
	for _, rec := range cat.Records {
		if rec.Hash != "" && rec.Volume != "" && !rec.IsLost() {
			bySize[rec.Size] = append(bySize[rec.Size], rec)
		}
	}

	var needHash, plainNew []*File
	for _, f := range candidates {
		if len(bySize[f.Size]) > 0 {
			needHash = append(needHash, f)
			d.HashedFiles++
			d.HashedBytes += SampleBytes(f.Size)
		} else {
			plainNew = append(plainNew, f)
		}
	}

	hashes := HashAll(needHash, opt.Jobs, onErr)
	claimed := make(map[*catalog.Record]bool)

	for _, f := range needHash {
		h, ok := hashes[f.Path]
		if !ok {
			plainNew = append(plainNew, f)
			continue
		}
		matched := d.pick(f, h, bySize[f.Size], claimed, mode, onErr)
		if matched == nil {
			plainNew = append(plainNew, f)
			continue
		}
		claimed[matched] = true
		d.Reused = append(d.Reused, Reuse{From: matched, To: f})
		d.ReusedBytes += f.Size
	}

	d.finishNew(plainNew)
	sortFiles(d.Modified)
	return d
}

// fileState is the verdict on a path that exists in both the scan and the
// catalog.
type fileState int

const (
	stateUnchanged fileState = iota
	stateModified
	stateSuspect
)

// suspect is a file whose size and mtime match the catalog but whose ctime or
// inode does not.
type suspect struct {
	file *File
	rec  *catalog.Record
}

// classify decides what a path's stat data says about it.
//
// Size and mtime are the primary signal. Ctime and inode corroborate: userspace
// can set mtime freely, and programs do — package managers, archive extraction,
// torrent clients and media managers all restore mtimes. A file rewritten with
// its mtime put back looks untouched to a size-and-mtime check, which would
// then never back it up again. Ctime cannot be set by userspace, so a
// disagreement means something happened, even if only a chmod.
func classify(rec *catalog.Record, f *File, opt Options) fileState {
	if rec.Size != f.Size || rec.ModTime != f.ModTime {
		return stateModified
	}
	// A zero on either side means the record predates these fields; fall back
	// to the mtime verdict rather than calling every old record suspect.
	if !opt.IgnoreCTime && rec.CTime != 0 && f.CTime != 0 && rec.CTime != f.CTime {
		return stateSuspect
	}
	if !opt.IgnoreInode && rec.Inode != 0 && f.Ino != 0 && rec.Inode != f.Ino {
		return stateSuspect
	}
	return stateUnchanged
}

// resolveSuspects settles files whose corroborating metadata disagreed, by
// reading them.
//
// This is where offline-media economics diverge from a disk-backed tool. Restic
// and borg treat a ctime mismatch as "changed" and simply re-read the file,
// which costs almost nothing. Here a false "changed" verdict costs a rewrite to
// media, so it is worth hashing the file to find out: a chmod sweep should not
// rewrite the archive, and a file whose mtime was restored after a real edit
// must not be missed.
func (d *Delta) resolveSuspects(suspects []suspect, opt Options, onErr func(string, error)) {
	if len(suspects) == 0 {
		return
	}
	d.Suspect = int64(len(suspects))

	jobs := max(opt.Jobs, 1)

	in := make(chan suspect)
	out := make(chan verdict)
	var wg sync.WaitGroup
	for range jobs {
		wg.Go(func() {
			for s := range in {
				out <- check(s, onErr)
			}
		})
	}
	go func() {
		for _, s := range suspects {
			in <- s
		}
		close(in)
		wg.Wait()
		close(out)
	}()

	for v := range out {
		d.HashedFiles++
		d.HashedBytes += v.read
		switch {
		case v.unknown:
			// No stored digest to compare against, so the safe answer is to
			// treat it as changed and write it again.
			d.Unverifiable++
			d.Modified = append(d.Modified, v.s.file)
			d.ModifiedBytes += v.s.file.Size
		case v.changed:
			d.CaughtByCTime++
			d.Modified = append(d.Modified, v.s.file)
			d.ModifiedBytes += v.s.file.Size
		default:
			d.UnchangedFiles++
			d.UnchangedBytes += v.s.file.Size
			d.Refreshed = append(d.Refreshed, v.s.file)
		}
	}
	sortFiles(d.Refreshed)
}

// verdict is what reading a suspect settled.
type verdict struct {
	s       suspect
	changed bool
	unknown bool
	read    int64
}

func check(s suspect, onErr func(string, error)) (v verdict) {
	v.s = s
	switch {
	case s.rec.FullHash != "":
		h, err := FullHash(s.file.OpenPath())
		if err != nil {
			onErr(s.file.OpenPath(), err)
			v.unknown = true
			return v
		}
		v.read = s.file.Size
		v.changed = h != s.rec.FullHash
	case s.rec.Hash != "" && s.rec.HashKind == catalog.HashKindSample:
		h, err := SampleHash(s.file.OpenPath(), s.file.Size)
		if err != nil {
			onErr(s.file.OpenPath(), err)
			v.unknown = true
			return v
		}
		v.read = SampleBytes(s.file.Size)
		v.changed = h != s.rec.Hash
	default:
		v.unknown = true
	}
	return v
}

// pick chooses the record whose content matches f, confirming against a
// whole-file digest when one is available and MatchFull is in force.
//
// The sampled digest covers a file's size and its first and last mebibyte. Two
// files that differ only in the middle would collide, which for a rename means
// restoring the wrong bytes under the right name. Confirming costs one full
// read of a candidate that has already been shortlisted, which is a tiny subset
// of the scan.
func (d *Delta) pick(f *File, sampled string, pool []*catalog.Record, claimed map[*catalog.Record]bool, mode MatchMode, onErr func(string, error)) *catalog.Record {
	var shortlist []*catalog.Record
	for _, rec := range pool {
		if claimed[rec] || rec.HashKind != catalog.HashKindSample || rec.Hash != sampled {
			continue
		}
		shortlist = append(shortlist, rec)
	}
	if len(shortlist) == 0 {
		return nil
	}

	if mode == MatchFull {
		confirmable := false
		for _, rec := range shortlist {
			if rec.FullHash != "" {
				confirmable = true
				break
			}
		}
		if confirmable {
			full, err := FullHash(f.OpenPath())
			if err != nil {
				onErr(f.OpenPath(), err)
				return nil
			}
			d.HashedBytes += f.Size
			for _, rec := range shortlist {
				if rec.FullHash == full {
					d.ConfirmedFull++
					return rec
				}
			}
			// A sampled collision that the full digest disproved: write it.
			return nil
		}
	}

	// Several records can share a size and digest — genuine duplicate content.
	// Prefer one that kept its filename, borrowing rclone's "leaf" strategy,
	// then the most recent version, whose volume is likeliest to be handy.
	leaf := filepath.Base(f.Path)
	best := shortlist[0]
	for _, rec := range shortlist[1:] {
		bestLeaf := filepath.Base(best.Path) == leaf
		recLeaf := filepath.Base(rec.Path) == leaf
		if recLeaf != bestLeaf {
			if recLeaf {
				best = rec
			}
			continue
		}
		if rec.Gen > best.Gen {
			best = rec
		}
	}
	return best
}

func (d *Delta) finishNew(files []*File) {
	d.New = files
	for _, f := range files {
		d.NewBytes += f.Size
	}
	sortFiles(d.New)
}

func sortFiles(f []*File) {
	sort.Slice(f, func(i, j int) bool { return f[i].Path < f[j].Path })
}
