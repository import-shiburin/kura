package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"kura/internal/catalog"
	"kura/internal/plan"
	"kura/internal/scan"
)

func pct(part, whole int64) string {
	if whole == 0 {
		return "  0.0%"
	}
	return fmt.Sprintf("%5.1f%%", 100*float64(part)/float64(whole))
}

// writeReport prints the human-facing summary of a cycle.
func writeReport(w io.Writer, p *plan.Plan, d *scan.Delta, stats *scan.Stats, allowSplit bool) {
	s := p.Summary
	fmt.Fprintf(w, "\n=== SCAN ===\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  regular files\t%d\t%s\n", stats.Files, plan.FormatBytes(stats.Bytes))
	fmt.Fprintf(tw, "  directories\t%d\t\n", stats.Dirs)
	fmt.Fprintf(tw, "  symlinks\t%d\t\n", stats.Symlinks)
	fmt.Fprintf(tw, "  hardlinked dupes\t%d\t(data stored once)\n", stats.HardLinks)
	fmt.Fprintf(tw, "  excluded\t%d\t\n", stats.Excluded)
	fmt.Fprintf(tw, "  unreadable\t%d\t\n", stats.Errors)
	tw.Flush()
	for _, r := range stats.SkippedRoots {
		fmt.Fprintf(w, "  ! root %s skipped: already covered by another root\n", r)
	}

	fmt.Fprintf(w, "\n=== DELTA vs CATALOG (generation %d) ===\n", p.Generation-1)
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  unchanged\t%d\t%s\t%s of scan\n", s.UnchangedFiles, plan.FormatBytes(s.UnchangedBytes), pct(s.UnchangedBytes, stats.Bytes))
	fmt.Fprintf(tw, "  new\t%d\t%s\t%s of scan\n", s.NewFiles, plan.FormatBytes(s.NewBytes), pct(s.NewBytes, stats.Bytes))
	fmt.Fprintf(tw, "  modified\t%d\t%s\t%s of scan\n", s.ModifiedFiles, plan.FormatBytes(s.ModifiedBytes), pct(s.ModifiedBytes, stats.Bytes))
	fmt.Fprintf(tw, "  already written\t%d\t%s\tmoved, copied, or re-added\n", s.ReusedFiles, plan.FormatBytes(s.ReusedBytes))
	fmt.Fprintf(tw, "  deleted\t%d\t%s\ttombstoned, data retained\n", s.DeletedFiles, plan.FormatBytes(s.DeletedBytes))
	tw.Flush()
	if s.HashedFiles > 0 {
		fmt.Fprintf(w, "  (opened %d files to resolve matches, reading %s", s.HashedFiles, plan.FormatBytes(s.HashedBytes))
		if d.ConfirmedFull > 0 {
			fmt.Fprintf(w, "; %d confirmed against a full digest", d.ConfirmedFull)
		}
		fmt.Fprintf(w, ")\n")
	}
	if s.ReplacingLost > 0 {
		// Called out rather than buried in the modified count: this measures
		// damage, not activity, and it is the whole visible effect of the
		// self-healing rule in DESIGN §5.1.
		fmt.Fprintf(w, "  %d of those rewrites are replacing copies recorded as lost,\n", s.ReplacingLost)
		fmt.Fprintf(w, "      not files that changed. Check the volumes they were on.\n")
	}
	if d.Suspect > 0 {
		fmt.Fprintf(w, "  %d files had a ctime or inode mismatch despite matching size and mtime:\n", d.Suspect)
		fmt.Fprintf(w, "      %d genuinely changed (an mtime-only check would have missed these)\n", d.CaughtByCTime)
		fmt.Fprintf(w, "      %d verified identical, metadata refreshed, nothing rewritten\n", int64(len(d.Refreshed)))
		if d.Unverifiable > 0 {
			fmt.Fprintf(w, "      %d had no stored digest to check against, rewritten to be safe\n", d.Unverifiable)
		}
	}

	fmt.Fprintf(w, "\n=== TO WRITE ===\n")
	fmt.Fprintf(w, "  %s in %d files, grouped into %d items\n", plan.FormatBytes(s.WriteBytes), s.NewFiles+s.ModifiedFiles, s.Items)
	if s.ReusedBytes > 0 {
		fmt.Fprintf(w, "  %s avoided; those bytes are already written\n", plan.FormatBytes(s.ReusedBytes))
	}

	fmt.Fprintf(w, "\n=== VOLUMES ===\n")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  VOLUME\tUSED\tFREE\tFILL\tITEMS\tSPLIT\n")
	for _, v := range p.Volumes {
		kind := ""
		if v.Existing {
			kind = " (carried over)"
		}
		fmt.Fprintf(tw, "  %s%s\t%s\t%s\t%s\t%d\t%d\n",
			v.ID, kind, plan.FormatBytes(v.Used), plan.FormatBytes(v.Free()),
			pct(v.Used, v.Capacity), len(v.Items), len(v.Splits))
	}
	tw.Flush()

	fmt.Fprintf(w, "\n=== FILL EFFICIENCY ===\n")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  volumes in this run\t%d\t(%d new, %d carried over)\n", s.VolumesTotal, s.VolumesNew, s.VolumesExisting)
	fmt.Fprintf(tw, "  usable capacity\t%s\t\n", plan.FormatBytes(s.CapacityBytes))
	fmt.Fprintf(tw, "  held as headroom\t%s\t%s of raw\n", plan.FormatBytes(s.ReservedBytes), pct(s.ReservedBytes, s.CapacityBytes+s.ReservedBytes))
	fmt.Fprintf(tw, "  written\t%s\t%s of usable\n", plan.FormatBytes(s.WriteBytes), pct(s.WriteBytes, s.CapacityBytes))
	fmt.Fprintf(tw, "  left unused\t%s\t%s of usable\n", plan.FormatBytes(s.UnusedBytes), pct(s.UnusedBytes, s.CapacityBytes))
	if s.VolumesTotal > 0 {
		fmt.Fprintf(tw, "  unused per volume\t%s\t\n", plan.FormatBytes(s.UnusedBytes/int64(s.VolumesTotal)))
	}
	if allowSplit {
		fmt.Fprintf(tw, "  files split\t%d\t\n", s.SplitFiles)
		fmt.Fprintf(tw, "  volumes saved by splitting\t%d\t\n", s.VolumesSavedBySplit)
	} else {
		fmt.Fprintf(tw, "  files split\t0\t(-allow-split is off)\n")
	}
	tw.Flush()

	// The last volume is expected to be partly empty; it is not stranded space,
	// since a later run can mount it and append. Judging fill efficiency on the
	// closed volumes alone is the honest measure.
	if len(p.Volumes) > 1 {
		var closed, closedCap int64
		for _, v := range p.Volumes[:len(p.Volumes)-1] {
			closed += v.Free()
			closedCap += v.Capacity
		}
		fmt.Fprintf(w, "\n  Excluding the final (still-appendable) volume: %s stranded across %d volumes, %s.\n",
			plan.FormatBytes(closed), len(p.Volumes)-1, pct(closed, closedCap))
	}

	if len(p.Warnings) > 0 {
		fmt.Fprintf(w, "\n=== WARNINGS ===\n")
		for _, warn := range p.Warnings {
			fmt.Fprintf(w, "  ! %s\n", warn)
		}
	}
}

// writeHistory prints the generation timeline and what the tree looked like at
// each point, which is the readable form of the property that nothing is ever
// reclaimed: every row is a state still restorable from what is on the shelf.
func writeHistory(w io.Writer, cat *catalog.Catalog) {
	latest := cat.MaxGen()
	if latest == 0 {
		fmt.Fprintf(w, "\n=== HISTORY ===\n  catalog is empty\n")
		return
	}
	dates := make(map[int]string, len(cat.Header.Timeline))
	volumes := make(map[int][]string, len(cat.Header.Timeline))
	for _, g := range cat.Header.Timeline {
		dates[g.Gen] = g.Written
		volumes[g.Gen] = g.Volumes
	}

	fmt.Fprintf(w, "\n=== HISTORY ===\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  GEN\tWRITTEN\tFILES\tSIZE\tADDED\tREMOVED\tLOST\tVOLUMES\n")
	for g := 1; g <= latest; g++ {
		state := cat.At(g)
		var bytes, added, removed, lost int64
		for _, r := range state {
			bytes += r.Size
			if r.Gen == g {
				added++
			}
			if r.IsLost() {
				lost++
			}
		}
		for _, r := range cat.Records {
			if r.DeletedGen == g {
				removed++
			}
		}
		when := dates[g]
		if when == "" {
			when = "-"
		}
		fmt.Fprintf(tw, "  %d\t%s\t%d\t%s\t+%d\t-%d\t%d\t%s\n",
			g, when, len(state), plan.FormatBytes(bytes), added, removed, lost, strings.Join(volumes[g], " "))
	}
	tw.Flush()
	fmt.Fprintf(w, "  Any row above is restorable except for its LOST column: deleted data\n")
	fmt.Fprintf(w, "  was never reclaimed, but damaged or reclaimed volumes take history with them.\n")
}

// writeStateAt prints the tree as it stood at one generation, grouped by the
// volumes a restore of that state would need, and returns how many objects
// cannot be produced.
//
// The count is the point of the whole exercise. A point-in-time query that
// quietly returned a short tree would look like a successful restore of a
// smaller archive; saying "1,247 objects unavailable" is the difference
// (DESIGN §5.1).
func writeStateAt(w io.Writer, cat *catalog.Catalog, gen int) int64 {
	state := cat.At(gen)
	if len(state) == 0 {
		fmt.Fprintf(w, "\n=== STATE AT GENERATION %d ===\n  nothing recorded at that generation\n", gen)
		return 0
	}

	type tally struct {
		files, lostFiles int
		bytes, lostBytes int64
	}
	byVolume := make(map[string]tally)
	var total, lostBytes int64
	var lostFiles int64
	lostReasons := make(map[catalog.LostReason]int64)

	for _, r := range state {
		total += r.Size
		e := byVolume[r.Volume]
		e.files++
		e.bytes += r.Size
		if r.IsLost() {
			lostFiles++
			lostBytes += r.Size
			e.lostFiles++
			e.lostBytes += r.Size
			lostReasons[r.Lost.Reason]++
		}
		byVolume[r.Volume] = e
	}

	fmt.Fprintf(w, "\n=== STATE AT GENERATION %d ===\n", gen)
	fmt.Fprintf(w, "  %d files, %s\n\n", len(state), plan.FormatBytes(total))
	ids := make([]string, 0, len(byVolume))
	for id := range byVolume {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  VOLUME\tFILES\tSIZE\tUNAVAILABLE\n")
	for _, id := range ids {
		label := id
		if label == "" {
			label = "(no volume recorded)"
		}
		e := byVolume[id]
		unavailable := ""
		if e.lostFiles > 0 {
			unavailable = fmt.Sprintf("%d (%s)", e.lostFiles, plan.FormatBytes(e.lostBytes))
		}
		fmt.Fprintf(tw, "  %s\t%d\t%s\t%s\n", label, e.files, plan.FormatBytes(e.bytes), unavailable)
	}
	tw.Flush()

	if lostFiles == 0 {
		fmt.Fprintf(w, "\n  Mount %s to reconstruct this state.\n", count(int64(len(ids)), "volume"))
		return 0
	}
	fmt.Fprintf(w, "\n  %s (%s) UNAVAILABLE: %s.\n",
		count(lostFiles, "object"), plan.FormatBytes(lostBytes), describeLoss(lostReasons))
	fmt.Fprintf(w, "  A restore of this generation will be incomplete. The remaining %s\n",
		count(int64(len(state))-lostFiles, "object"))
	fmt.Fprintf(w, "  are still on the %s above.\n", count(int64(len(ids)), "volume"))
	return lostFiles
}

// count renders "1 object" / "2 objects", because these lines are read by
// someone deciding whether a restore is worth attempting.
func count(n int64, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// describeLoss keeps the two reasons apart in the one place an operator reads
// them. Reclaimed was chosen and is permanent; media was suffered and may come
// back on another drive.
func describeLoss(reasons map[catalog.LostReason]int64) string {
	var parts []string
	if n := reasons[catalog.LostMedia]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d unreadable on their media", n))
	}
	if n := reasons[catalog.LostReclaimed]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d dropped by a reclamation", n))
	}
	for reason, n := range reasons {
		if reason != catalog.LostMedia && reason != catalog.LostReclaimed {
			parts = append(parts, fmt.Sprintf("%d marked %q", n, reason))
		}
	}
	return strings.Join(parts, ", ")
}

// topDirs reports the largest directories in the write set, which is usually
// the fastest way to see what is driving a cycle's size.
//
// Depth is counted from the scan root rather than from /, so that scanning
// /storage and scanning /mnt/backups/storage group at the same level.
func topDirs(files []*scan.File, roots []string, depth, n int, w io.Writer) {
	if len(files) == 0 {
		return
	}
	byDir := make(map[string]int64)
	counts := make(map[string]int64)
	for _, f := range files {
		byDir[groupKey(f.Path, roots, depth)] += f.Size
		counts[groupKey(f.Path, roots, depth)]++
	}
	keys := make([]string, 0, len(byDir))
	for k := range byDir {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return byDir[keys[i]] > byDir[keys[j]] })
	if len(keys) > n {
		keys = keys[:n]
	}

	fmt.Fprintf(w, "\n=== LARGEST IN WRITE SET (%d levels below the root) ===\n", depth)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, k := range keys {
		fmt.Fprintf(tw, "  %s\t%s\t%d files\n", k, plan.FormatBytes(byDir[k]), counts[k])
	}
	tw.Flush()
}

// groupKey truncates a path to the given number of components below whichever
// root contains it.
func groupKey(path string, roots []string, depth int) string {
	root := ""
	for _, r := range roots {
		r = strings.TrimSuffix(r, "/")
		if (path == r || strings.HasPrefix(path, r+"/")) && len(r) > len(root) {
			root = r
		}
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(path, root), "/")
	parts := strings.Split(rest, "/")
	// Aggregate by directory, so drop the filename before truncating; a file
	// shallower than depth is reported under its parent, not as itself.
	parts = parts[:len(parts)-1]
	if len(parts) > depth {
		parts = parts[:depth]
	}
	if len(parts) == 0 {
		return root + "/"
	}
	return root + "/" + strings.Join(parts, "/")
}
