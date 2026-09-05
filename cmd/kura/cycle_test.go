package main

import (
	"os"
	"path/filepath"
	"testing"

	"kura/internal/catalog"
	"kura/internal/plan"
	"kura/internal/scan"
)

const mb = int64(1e6)

// writeTree materialises a set of paths with deterministic content.
func writeTree(t *testing.T, root string, spec map[string]int) {
	t.Helper()
	for rel, size := range spec {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, size)
		for i := range buf {
			buf[i] = byte((i*7 + len(rel)) % 251)
		}
		if err := os.WriteFile(p, buf, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func moveFile(t *testing.T, from, to string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
}

func pathsOf(files []*scan.File) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}

func recPathsOf(recs []*catalog.Record) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.Path
	}
	return out
}

// catalogFromPlan stands in for the writer: after a plan is executed, every
// placed file becomes a catalog record carrying the volume it landed on and the
// sampled hash the writer computed while streaming it.
func catalogFromPlan(t *testing.T, p *plan.Plan, gen int) []*catalog.Record {
	t.Helper()
	var recs []*catalog.Record
	for _, v := range p.Volumes {
		for _, e := range v.Entries {
			h, err := scan.SampleHash(e.Path, e.Size)
			if err != nil {
				t.Fatalf("hashing %s: %v", e.Path, err)
			}
			recs = append(recs, &catalog.Record{
				Path: e.Path, Size: e.Size, ModTime: e.ModTime,
				Hash: h, HashKind: catalog.HashKindSample, Volume: v.ID, Gen: gen,
			})
		}
	}
	return recs
}

func planTree(t *testing.T, root string, cat *catalog.Catalog) (*plan.Plan, *scan.Delta) {
	t.Helper()
	files, stats, err := scan.Walk([]scan.Root{{Scan: root}}, scan.NewExcluder(nil, true), func(string, error) {})
	if err != nil {
		t.Fatal(err)
	}
	d := scan.Diff(files, cat, scan.Options{Match: scan.MatchSample, Jobs: 2}, func(string, error) {})
	items := plan.Group(d.ToWrite(), 200*gb, plan.Layout{})
	opt := plan.PackOptions{Capacity: 10 * mb, MaxBlanks: -1, NamePrefix: "LTO"}
	p := plan.Pack(items, nil, opt)
	p.Generation = cat.MaxGen() + 1
	p.Finalize(d, stats, items)
	return p, d
}

// TestIncrementalCycle is the end-to-end claim of the whole scheme: after a
// baseline, a routine cycle writes only genuinely new content, a moved file
// costs nothing, and everything else is settled from stat data alone.
func TestIncrementalCycle(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]int{
		"Movie/film01.mkv":        3 << 20,
		"Movie/film02.mkv":        2 << 20,
		"Anime/Show/S01/ep01.mkv": 1 << 20,
		"Anime/Show/S01/ep02.mkv": 1 << 20,
		"Docs/notes.md":           4096,
		"Docs/spec.md":            8192,
		"Retired/obsolete.bin":    512 << 10,
	})

	// --- Generation 1: the baseline. Everything is new.
	base, baseDelta := planTree(t, root, catalog.New())
	if baseDelta.UnchangedFiles != 0 {
		t.Errorf("baseline should have nothing unchanged, got %d", baseDelta.UnchangedFiles)
	}
	if got := int64(len(baseDelta.New)); got != 7 {
		t.Errorf("baseline should treat all 7 files as new, got %d", got)
	}

	recs := catalogFromPlan(t, base, 1)
	catPath := filepath.Join(t.TempDir(), "catalog.jsonl.gz")
	if err := catalog.Write(catPath, catalog.Header{Generation: 1}, recs); err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Load(catPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Records) != len(recs) {
		t.Fatalf("catalog round trip lost records: wrote %d, read %d", len(recs), len(cat.Records))
	}

	// --- Between cycles: one addition, one move, one edit, one deletion.
	writeTree(t, root, map[string]int{"Movie/film03.mkv": 5 << 20}) // new
	moveFile(t, filepath.Join(root, "Anime/Show/S01/ep02.mkv"),
		filepath.Join(root, "Anime/Show/Season 01/ep02.mkv")) // renamed
	writeTree(t, root, map[string]int{"Docs/spec.md": 9000}) // modified
	if err := os.Remove(filepath.Join(root, "Retired/obsolete.bin")); err != nil {
		t.Fatal(err)
	}

	// --- Generation 2.
	_, d := planTree(t, root, cat)

	if len(d.New) != 1 || filepath.Base(d.New[0].Path) != "film03.mkv" {
		t.Errorf("expected film03.mkv as the only new file, got %v", pathsOf(d.New))
	}
	if len(d.Modified) != 1 || filepath.Base(d.Modified[0].Path) != "spec.md" {
		t.Errorf("expected spec.md as the only modified file, got %v", pathsOf(d.Modified))
	}
	if len(d.Reused) != 1 {
		t.Fatalf("expected the moved episode to reuse its existing copy, got %d (new=%v)", len(d.Reused), pathsOf(d.New))
	}
	if d.Reused[0].From.Volume == "" {
		t.Error("a reuse must point at the volume already holding the data")
	}
	// Two paths stopped existing: the removed file, and the one the episode
	// moved out of. Both are tombstoned; the episode's bytes survive under the
	// new name, so only the removed file is gone in any practical sense.
	if len(d.Deleted) != 2 {
		t.Errorf("expected the removed file and the vacated path to be tombstoned, got %v", recPathsOf(d.Deleted))
	}
	var sawObsolete bool
	for _, r := range d.Deleted {
		if filepath.Base(r.Path) == "obsolete.bin" {
			sawObsolete = true
		}
	}
	if !sawObsolete {
		t.Errorf("obsolete.bin should be among the deletions, got %v", recPathsOf(d.Deleted))
	}
	if d.UnchangedFiles != 4 {
		t.Errorf("expected 4 untouched files, got %d", d.UnchangedFiles)
	}

	// The payoff: the cycle rewrites only the new file plus the edited one,
	// not the whole tree.
	wantWrite := int64(5<<20) + 9000
	if d.WriteBytes() != wantWrite {
		t.Errorf("cycle writes %d bytes, want %d", d.WriteBytes(), wantWrite)
	}
	if d.ReusedBytes != 1<<20 {
		t.Errorf("the move should have avoided %d bytes, credited %d", 1<<20, d.ReusedBytes)
	}
}

// TestDamagedVolumeIsReprotectedOnTheNextCycle is the self-healing loop end to
// end: verify marks what a failed volume held, and the very next plan — with no
// extra machinery and nothing changed on disk — re-sources exactly those files
// and leaves everything else alone.
func TestDamagedVolumeIsReprotectedOnTheNextCycle(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]int{
		"a/one.bin":   2 << 20,
		"a/two.bin":   2 << 20,
		"b/three.bin": 2 << 20,
		"b/four.bin":  2 << 20,
	})

	base, _ := planTree(t, root, catalog.New())
	cat := catalogOf(catalogFromPlan(t, base, 1)...)
	if len(cat.Records) != 4 {
		t.Fatalf("expected 4 records after the baseline, got %d", len(cat.Records))
	}

	// A routine cycle over an unchanged tree writes nothing.
	if _, quiet := planTree(t, root, cat); quiet.WriteBytes() != 0 {
		t.Fatalf("an unchanged tree should write nothing, got %d bytes", quiet.WriteBytes())
	}

	// Verify finds one volume unreadable and marks every version it held.
	damaged := cat.Records[0].Volume
	marked := cat.OnVolume(damaged)
	if len(marked) == 0 {
		t.Fatal("the baseline put nothing on the volume being damaged")
	}
	var lostBytes int64
	for _, r := range marked {
		r.MarkLost(catalog.Lost{Gen: 2, Reason: catalog.LostMedia, Detail: damaged + " unreadable during verify"})
		lostBytes += r.Size
	}

	// Nothing on disk changed, but the next cycle re-sources exactly those.
	_, d := planTree(t, root, cat)

	if d.ReplacingLost != int64(len(marked)) {
		t.Errorf("expected %d files re-sourced, got %d", len(marked), d.ReplacingLost)
	}
	if d.WriteBytes() != lostBytes {
		t.Errorf("expected %d bytes rewritten, got %d", lostBytes, d.WriteBytes())
	}
	if int64(len(d.Modified)) != int64(len(marked)) {
		t.Errorf("only the lost files should be written, got %v", pathsOf(d.Modified))
	}
	if d.UnchangedFiles != int64(4-len(marked)) {
		t.Errorf("the files on healthy volumes should still settle as unchanged, got %d", d.UnchangedFiles)
	}
	// And no reuse quietly pointed a path back at the gone bytes.
	if len(d.Reused) != 0 {
		t.Errorf("nothing should have been matched against the damaged volume, got %d reuses", len(d.Reused))
	}
	for _, f := range d.Modified {
		if r, ok := cat.Lookup(f.Path); !ok || !r.IsLost() {
			t.Errorf("%s was rewritten but its record is not the lost one", f.Path)
		}
	}
}
