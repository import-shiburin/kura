package plan

import (
	"fmt"
	"sort"

	"kura/internal/scan"
)

// Volume is one unit of media in the plan: what the packer decided goes on it,
// before anything has been mounted or written.
type Volume struct {
	ID       string `json:"id"`
	Capacity int64  `json:"capacity"` // usable bytes, already net of Reserved
	Reserved int64  `json:"reserved"`
	Used     int64  `json:"used"`
	Existing bool   `json:"existing"` // a partially-filled volume carried over

	// Items is the grouped view, for reading the plan by eye.
	Items  []Item      `json:"items"`
	Splits []SplitPart `json:"splits,omitempty"`

	// Entries is the flat, authoritative list the writer consumes: every byte
	// range destined for this volume, in path order.
	Entries []Entry `json:"entries"`
}

// Entry is one file, or one fragment of a split file, bound for a volume.
type Entry struct {
	Path string `json:"path"`
	// Source is where the writer must read the bytes from, set only when that
	// differs from Path — which is the case when the plan was made against a
	// snapshot of the live tree.
	Source  string `json:"source,omitempty"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime"`
	Hash    string `json:"hash,omitempty"`

	// Container names the tar this file will be written inside, empty when it
	// gets an object of its own.
	//
	// The plan decides this rather than the writer because the decision needs
	// the item grouping, and Item.Files does not survive the trip through the
	// plan file — Entries is the authoritative list precisely so the same
	// bytes are not written twice. Recording it also makes a dry run able to
	// say exactly which containers a cycle will create.
	Container string `json:"container,omitempty"`

	// Offset, Length and the part numbers are set only for split files, where
	// Size is the fragment and FileSize is the whole file it belongs to.
	Offset   int64 `json:"offset,omitempty"`
	Length   int64 `json:"length,omitempty"`
	Part     int   `json:"part,omitempty"`
	OfParts  int   `json:"of_parts,omitempty"`
	FileSize int64 `json:"file_size,omitempty"`
}

// ReadFrom is where the writer must read an entry's bytes from: the snapshot it
// was planned against when there is one, otherwise the path itself.
func (e *Entry) ReadFrom() string {
	if e.Source != "" {
		return e.Source
	}
	return e.Path
}

// Split reports whether this entry is a fragment rather than a whole file.
func (e *Entry) Split() bool { return e.OfParts > 1 }

// Free is the space still available on the volume.
func (v *Volume) Free() int64 { return v.Capacity - v.Used }

// SplitPart is one fragment of a file deliberately divided across volumes to
// use up a hole that nothing else fits into.
//
// Source and Size describe the whole file, not the fragment: the writer has to
// know where to read from and how long the finished file is, and neither is
// recoverable from Path when the plan was made against a snapshot.
type SplitPart struct {
	Path    string `json:"path"`
	Source  string `json:"source,omitempty"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime"`
	Offset  int64  `json:"offset"`
	Length  int64  `json:"length"`
	Part    int    `json:"part"`
	OfParts int    `json:"of_parts"`
}

// splitOf builds a fragment descriptor carrying everything the writer needs.
func splitOf(f *scan.File, offset, length int64) SplitPart {
	return SplitPart{
		Path: f.Path, Source: f.Real, Size: f.Size, ModTime: f.ModTime,
		Offset: offset, Length: length,
	}
}

// Format versions the plan file the writer consumes.
const Format = "kura-plan/1"

// Plan is the packer's output.
type Plan struct {
	Format     string `json:"_format"`
	Generation int    `json:"generation"`
	Created    string `json:"created"`

	Volumes  []*Volume `json:"volumes"`
	Summary  Summary   `json:"summary"`
	Warnings []string  `json:"warnings,omitempty"`
}

// PackOptions controls bin packing.
type PackOptions struct {
	Capacity   int64 // raw capacity of a blank volume
	Headroom   int64 // bytes held back on every volume
	MaxBlanks  int   // -1 for unlimited
	AllowSplit bool
	SplitMin   int64 // smallest hole worth splitting a file into
	NamePrefix string
	StartIndex int

	// Layout is how files become objects. Packing needs it because a drained
	// file has to be re-costed in stored bytes, and because a plan has to say
	// exactly what the writer will create.
	Layout Layout

	// MetadataReserve is room kept on every volume for the things the volume
	// says about itself: its header, and the copies of the catalog that make it
	// readable with no external database.
	//
	// It is separate from Headroom because it is not slack — it is space that
	// will certainly be used. Without it a volume packed to its last byte has
	// nowhere to put its own index, and an oversized file split to fill the
	// remaining space guarantees that outcome every time.
	MetadataReserve int64
}

// DefaultMetadataReserve is the floor applied when a caller names none. It
// covers a header and a small catalog; a real archive should pass an estimate
// scaled to its own catalog, since that grows with the number of files.
const DefaultMetadataReserve = 1 << 20

// Pack places items onto volumes with First-Fit-Decreasing.
//
// FFD is used rather than filling in directory order because the difference is
// the entire fill-efficiency question: walking the tree and stopping at the
// first file that does not fit strands space proportional to the largest file,
// while FFD over a size distribution with a long small-file tail almost always
// finds something to top the volume off with.
func Pack(items []Item, existing []*Volume, opt PackOptions) *Plan {
	p := &Plan{Format: Format}
	p.Volumes = append(p.Volumes, existing...)

	reserve := opt.MetadataReserve
	if reserve <= 0 {
		reserve = DefaultMetadataReserve
	}
	usable := opt.Capacity - opt.Headroom - reserve
	if usable <= 0 {
		p.Warnings = append(p.Warnings,
			fmt.Sprintf("headroom and metadata reserve (%s) leave nothing of a %s volume",
				FormatBytes(opt.Headroom+reserve), FormatBytes(opt.Capacity)))
		return p
	}

	ordered := append([]Item(nil), items...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Size != ordered[j].Size {
			return ordered[i].Size > ordered[j].Size
		}
		return ordered[i].Name < ordered[j].Name
	})

	blanks := 0
	next := opt.StartIndex
	newVolume := func() *Volume {
		v := &Volume{
			ID:       fmt.Sprintf("%s%03d", opt.NamePrefix, next),
			Capacity: usable,
			Reserved: opt.Headroom + reserve,
		}
		next++
		blanks++
		p.Volumes = append(p.Volumes, v)
		return v
	}
	exhausted := func() bool { return opt.MaxBlanks >= 0 && blanks >= opt.MaxBlanks }

	for _, it := range ordered {
		if place(p.Volumes, it) {
			continue
		}
		if exhausted() {
			p.Warnings = append(p.Warnings,
				fmt.Sprintf("out of volumes with %s unplaced, starting at %s", FormatBytes(it.Size), it.Name))
			break
		}
		if it.Size > usable {
			// A single file larger than a whole volume; it has no choice but to
			// span. Group never produces an oversized directory item.
			if !opt.AllowSplit || len(it.Files) != 1 {
				p.Warnings = append(p.Warnings,
					fmt.Sprintf("%s is %s, larger than one volume; enable -allow-split to store it", it.Name, FormatBytes(it.Size)))
				continue
			}
			spreadOversized(p, it.Files[0], newVolume, exhausted)
			continue
		}
		v := newVolume()
		v.Items = append(v.Items, it)
		v.Used += it.Size
	}

	if opt.AllowSplit {
		p.Summary.VolumesSavedBySplit = compact(p, opt.SplitMin, opt.Layout)
	}
	return p
}

func place(volumes []*Volume, it Item) bool {
	for _, v := range volumes {
		if v.Free() >= it.Size {
			v.Items = append(v.Items, it)
			v.Used += it.Size
			return true
		}
	}
	return false
}

// spreadOversized lays a file that exceeds one volume across consecutive
// volumes.
//
// Because items are placed largest-first, such a file is usually the very first
// thing packed, so there may be no volume open yet.
func spreadOversized(p *Plan, f *scan.File, newVolume func() *Volume, exhausted func() bool) {
	var placed []*Volume
	var offset int64
	for offset < f.Size {
		var v *Volume
		if len(p.Volumes) > 0 {
			v = p.Volumes[len(p.Volumes)-1]
		}
		if v == nil || v.Free() == 0 {
			if exhausted() {
				p.Warnings = append(p.Warnings,
					fmt.Sprintf("out of volumes partway through %s", f.Path))
				return
			}
			v = newVolume()
		}
		n := min(v.Free(), f.Size-offset)
		v.Splits = append(v.Splits, splitOf(f, offset, n))
		v.Used += n
		offset += n
		placed = append(placed, v)
	}
	for i, v := range placed {
		s := &v.Splits[len(v.Splits)-1]
		s.Part, s.OfParts = i+1, len(placed)
	}
}

// compact tries to dissolve the last volume into the space left on earlier
// ones, first by breaking up directory groups and then, if that is not enough,
// by splitting individual files. Each successful pass removes one volume from
// the run.
func compact(p *Plan, splitMin int64, lay Layout) int {
	saved := 0
	for len(p.Volumes) > 1 {
		last := p.Volumes[len(p.Volumes)-1]
		if last.Existing || len(last.Splits) > 0 {
			break
		}
		if !drain(last, p.Volumes[:len(p.Volumes)-1], splitMin, lay) {
			break
		}
		p.Volumes = p.Volumes[:len(p.Volumes)-1]
		saved++
	}
	return saved
}

// move is one whole file relocated during a drain.
type move struct {
	volume int
	file   *scan.File
}

// frag is one fragment of a file that had to be split during a drain.
type frag struct {
	volume int
	offset int64
	length int64
}

// fragmented pairs a file with the fragments it was cut into.
type fragmented struct {
	file  *scan.File
	frags []frag
}

// drain attempts to move every file off src onto dst. The whole move is
// simulated against a scratch copy of the free space and only committed once it
// fully succeeds, so a failed attempt leaves the plan untouched.
func drain(src *Volume, dst []*Volume, splitMin int64, lay Layout) bool {
	var files []*scan.File
	for _, it := range src.Items {
		files = append(files, it.Files...)
	}
	if len(files) == 0 {
		return true
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].Size != files[j].Size {
			return files[i].Size > files[j].Size
		}
		return files[i].Path < files[j].Path
	})

	// A file moved out of its item is re-costed: on its own it becomes an item
	// of one, with its own container framing if it is small enough to be
	// bundled. Costing it at its source size would let a drain claim space it
	// does not have.
	stored := make(map[*scan.File]int64, len(files))
	for _, f := range files {
		stored[f] = lay.storedSize(f)
	}

	free := make([]int64, len(dst))
	var total int64
	for i, v := range dst {
		free[i] = v.Free()
		total += free[i]
	}
	if total < src.Used {
		return false
	}

	// Pass one: place whole files wherever they fit outright.
	var moves []move
	var leftovers []*scan.File
	for _, f := range files {
		placed := false
		for i := range dst {
			if free[i] >= stored[f] {
				free[i] -= stored[f]
				moves = append(moves, move{volume: i, file: f})
				placed = true
				break
			}
		}
		if !placed {
			leftovers = append(leftovers, f)
		}
	}

	// Pass two: cut whatever is left into the remaining holes. A hole smaller
	// than splitMin is left alone; chasing the last few gigabytes is not worth
	// making a file span one more volume.
	var cuts []fragmented
	for _, f := range leftovers {
		var frags []frag
		var offset int64
		for offset < f.Size {
			idx := -1
			for i := range dst {
				if free[i] >= splitMin {
					idx = i
					break
				}
			}
			if idx < 0 {
				return false
			}
			n := min(free[idx], f.Size-offset)
			free[idx] -= n
			frags = append(frags, frag{volume: idx, offset: offset, length: n})
			offset += n
		}
		cuts = append(cuts, fragmented{file: f, frags: frags})
	}

	for _, m := range moves {
		v := dst[m.volume]
		v.Items = append(v.Items, lay.makeItem(m.file.Path, []*scan.File{m.file}, false))
	}
	for _, c := range cuts {
		for i, fr := range c.frags {
			sp := splitOf(c.file, fr.offset, fr.length)
			sp.Part, sp.OfParts = i+1, len(c.frags)
			dst[fr.volume].Splits = append(dst[fr.volume].Splits, sp)
		}
	}
	for i, v := range dst {
		v.Used = v.Capacity - free[i]
	}
	return true
}
