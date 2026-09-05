package plan

import (
	"fmt"
	"sort"

	"kura/internal/scan"
)

// Summary is the headline arithmetic, both for the printed report and for the
// plan file.
type Summary struct {
	ScannedFiles int64 `json:"scanned_files"`
	ScannedBytes int64 `json:"scanned_bytes"`

	UnchangedFiles int64 `json:"unchanged_files"`
	UnchangedBytes int64 `json:"unchanged_bytes"`
	NewFiles       int64 `json:"new_files"`
	NewBytes       int64 `json:"new_bytes"`
	ModifiedFiles  int64 `json:"modified_files"`
	ModifiedBytes  int64 `json:"modified_bytes"`
	ReusedFiles    int64 `json:"reused_files"`
	ReusedBytes    int64 `json:"reused_bytes"`
	DeletedFiles   int64 `json:"deleted_files"`
	DeletedBytes   int64 `json:"deleted_bytes"`

	// ReplacingLost is how much of the modified count is re-protecting copies
	// that are gone rather than following a change on disk.
	ReplacingLost int64 `json:"replacing_lost,omitempty"`

	HashedFiles int64 `json:"hashed_files"`
	HashedBytes int64 `json:"hashed_bytes"`

	WriteBytes int64 `json:"write_bytes"`
	Items      int64 `json:"items"`

	// Containers is how many tars the write will create. Together with the
	// object count it is the figure that decides how long a volume takes to
	// write, since the medium charges per file rather than per byte.
	Containers int64 `json:"containers"`

	VolumesTotal        int   `json:"volumes_total"`
	VolumesNew          int   `json:"volumes_new"`
	VolumesExisting     int   `json:"volumes_existing"`
	CapacityBytes       int64 `json:"capacity_bytes"`
	ReservedBytes       int64 `json:"reserved_bytes"`
	UnusedBytes         int64 `json:"unused_bytes"`
	SplitFiles          int64 `json:"split_files"`
	VolumesSavedBySplit int   `json:"volumes_saved_by_split"`
}

// FormatBytes renders a byte count in decimal units, the way media capacities
// are quoted.
func FormatBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "kMGTP"[exp])
}

// Finalize fills in the plan summary from the delta and the packed volumes,
// and settles the layout: which files share a container, and what each one is
// called.
//
// The layout was settled by Group; this only flattens it into the entry list
// the plan file carries, because Item.Files does not survive serialisation and
// a writer reading the plan back has no way to re-derive the grouping.
func (p *Plan) Finalize(d *scan.Delta, stats *scan.Stats, items []Item) {
	s := &p.Summary
	s.ScannedFiles = stats.Files
	s.ScannedBytes = stats.Bytes
	s.UnchangedFiles = d.UnchangedFiles
	s.UnchangedBytes = d.UnchangedBytes
	s.NewFiles = int64(len(d.New))
	s.NewBytes = d.NewBytes
	s.ModifiedFiles = int64(len(d.Modified))
	s.ModifiedBytes = d.ModifiedBytes
	s.ReusedFiles = int64(len(d.Reused))
	s.ReusedBytes = d.ReusedBytes
	s.DeletedFiles = int64(len(d.Deleted))
	s.DeletedBytes = d.DeletedBytes
	s.ReplacingLost = d.ReplacingLost
	s.HashedFiles = d.HashedFiles
	s.HashedBytes = d.HashedBytes
	s.WriteBytes = d.WriteBytes()
	s.Items = int64(len(items))

	split := make(map[string]bool)
	for _, v := range p.Volumes {
		s.VolumesTotal++
		if v.Existing {
			s.VolumesExisting++
		} else {
			s.VolumesNew++
		}
		s.CapacityBytes += v.Capacity
		s.ReservedBytes += v.Reserved
		s.UnusedBytes += v.Free()
		for _, sp := range v.Splits {
			split[sp.Path] = true
		}
		v.buildEntries()
	}
	s.SplitFiles = int64(len(split))
	for _, v := range p.Volumes {
		seen := make(map[string]bool)
		for _, e := range v.Entries {
			if e.Container != "" && !seen[e.Container] {
				seen[e.Container] = true
				s.Containers++
			}
		}
	}
}

// buildEntries flattens the assigned layout into the single path-ordered list
// the writer works from. Path order is what keeps an LTFS copy sequential
// across the cartridge.
//
// It reads the container assignment rather than making one. Deciding it twice —
// once for sizing and once for writing — is how the two would drift apart, and
// a plan that says a file is in one container while the writer puts it in
// another is a file nothing can find.
func (v *Volume) buildEntries() {
	v.Entries = v.Entries[:0]
	for _, it := range v.Items {
		for _, f := range it.Loose {
			v.Entries = append(v.Entries, entryOf(f, ""))
		}
		for _, c := range it.Containers {
			for _, f := range c.Files {
				v.Entries = append(v.Entries, entryOf(f, c.Name))
			}
		}
	}
	for _, sp := range v.Splits {
		v.Entries = append(v.Entries, Entry{
			Path: sp.Path, Source: sp.Source, ModTime: sp.ModTime,
			Size: sp.Length, FileSize: sp.Size, Offset: sp.Offset, Length: sp.Length,
			Part: sp.Part, OfParts: sp.OfParts,
		})
	}
	sort.Slice(v.Entries, func(i, j int) bool {
		if v.Entries[i].Path != v.Entries[j].Path {
			return v.Entries[i].Path < v.Entries[j].Path
		}
		return v.Entries[i].Offset < v.Entries[j].Offset
	})
}

func entryOf(f *scan.File, container string) Entry {
	return Entry{
		Path: f.Path, Source: f.Real, Size: f.Size, ModTime: f.ModTime,
		Hash: f.Hash, Container: container,
	}
}
