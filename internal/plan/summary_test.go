package plan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kura/internal/catalog"
	"kura/internal/scan"
)

// writeTree materialises a set of paths with deterministic content.
func writeTree(t *testing.T, root string, spec map[string]int) {
	t.Helper()
	for rel, size := range spec {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBuildEntriesCoversItemsAndSplits(t *testing.T) {
	item := Layout{}.makeItem("/storage/Show/", []*scan.File{
		{Path: "/storage/Show/b.mkv", Size: 2 * gb, ModTime: 10, Hash: "bb"},
		{Path: "/storage/Show/a.mkv", Size: 1 * gb, ModTime: 20},
	}, true)
	v := &Volume{
		ID:       "LTO001",
		Capacity: 2500 * gb,
		Items:    []Item{item},
		Splits: []SplitPart{
			{Path: "/storage/big.iso", Size: 200 * gb, Offset: 0, Length: 100 * gb, Part: 1, OfParts: 2},
		},
	}
	v.buildEntries()

	if len(v.Entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(v.Entries))
	}
	// Entries must come out in path order so an LTFS copy stays sequential.
	for i := 1; i < len(v.Entries); i++ {
		if v.Entries[i-1].Path > v.Entries[i].Path {
			t.Errorf("entries are not path-ordered: %q before %q", v.Entries[i-1].Path, v.Entries[i].Path)
		}
	}
	if got := v.Entries[0]; got.Path != "/storage/Show/a.mkv" || got.Split() {
		t.Errorf("first entry = %+v, want the whole file /storage/Show/a.mkv", got)
	}

	var split *Entry
	for i := range v.Entries {
		if v.Entries[i].Split() {
			split = &v.Entries[i]
		}
	}
	if split == nil {
		t.Fatal("the split fragment did not survive into the entry list")
	}
	if split.Length != 100*gb || split.OfParts != 2 {
		t.Errorf("split entry = %+v, want 100 GB part 1 of 2", *split)
	}

	// Rebuilding must be idempotent rather than accumulating duplicates.
	v.buildEntries()
	if len(v.Entries) != 3 {
		t.Errorf("buildEntries is not idempotent: %d entries after a second call", len(v.Entries))
	}
}

func TestFinalizeOnAnEmptyPlan(t *testing.T) {
	p := &Plan{Format: Format}
	p.Finalize(new(scan.Delta), new(scan.Stats), nil)

	if p.Summary.VolumesTotal != 0 || p.Summary.WriteBytes != 0 {
		t.Errorf("an empty plan should summarise to zeroes, got %+v", p.Summary)
	}
}

// TestFinalizeCarriesTheLostCount keeps the damage figure out of the ordinary
// modified count when a report is read: a cycle that rewrites 1,247 files
// because a volume failed is a different event from one that rewrites 1,247
// files somebody edited.
func TestFinalizeCarriesTheLostCount(t *testing.T) {
	d := &scan.Delta{
		Modified:      []*scan.File{mkfile("/storage/x", 100)},
		ModifiedBytes: 100,
		ReplacingLost: 1,
	}
	p := &Plan{Format: Format}
	p.Finalize(d, new(scan.Stats), nil)

	if p.Summary.ReplacingLost != 1 {
		t.Errorf("the lost count did not reach the summary, got %d", p.Summary.ReplacingLost)
	}
	if p.Summary.ModifiedFiles != 1 {
		t.Errorf("a lost rewrite is still a modified file, got %d", p.Summary.ModifiedFiles)
	}
}

// TestPlanCarriesSourcePaths checks the writer is told where to actually read
// from when the plan was made against a snapshot.
func TestPlanCarriesSourcePaths(t *testing.T) {
	base := t.TempDir()
	live := filepath.Join(base, "storage")
	snap := filepath.Join(base, "storage/.zfs/snapshot/auto-1")
	writeTree(t, snap, map[string]int{"Movie/film.mkv": 4096})

	files, stats, err := scan.Walk([]scan.Root{{Scan: snap, As: live}}, scan.NewExcluder(nil, true), func(string, error) {})
	if err != nil {
		t.Fatal(err)
	}
	d := scan.Diff(files, catalog.New(), scan.Options{Match: scan.MatchOff, Jobs: 1}, func(string, error) {})
	items := Group(d.ToWrite(), 200*gb, Layout{})
	p := Pack(items, nil, PackOptions{Capacity: 10 * mb, MaxBlanks: -1, NamePrefix: "LTO"})
	p.Finalize(d, stats, items)

	if len(p.Volumes) != 1 || len(p.Volumes[0].Entries) != 1 {
		t.Fatalf("expected one entry on one volume, got %d volumes", len(p.Volumes))
	}
	e := p.Volumes[0].Entries[0]
	if !strings.HasPrefix(e.Path, live+"/") {
		t.Errorf("entry path %q should be the canonical identity", e.Path)
	}
	if !strings.HasPrefix(e.Source, snap+"/") {
		t.Errorf("entry source %q should point into the snapshot", e.Source)
	}
}

// The deleted paths have to survive into the plan file, not just their count.
//
// A deletion is discovered by the scan and applied by the writer, and nothing
// carries the scan's findings between the two except this file. While only the
// count travelled, `kura write` had no way to record that anything had gone
// away — the report said "tombstoned, data retained" and nothing was
// tombstoned.
func TestPlanCarriesTheDeletedPathsNotJustTheCount(t *testing.T) {
	cat := catalog.New()
	cat.Add(
		&catalog.Record{Path: "/storage/gone-a", Size: 10, Volume: "V001", Gen: 1},
		&catalog.Record{Path: "/storage/gone-b", Size: 20, Volume: "V001", Gen: 1},
		&catalog.Record{Path: "/storage/stays", Size: 30, Volume: "V001", Gen: 1},
	)
	// Only "stays" is still on disk, so the other two are deletions.
	files := []*scan.File{{Path: "/storage/stays", Size: 30}}
	d := scan.Diff(files, cat, scan.Options{Match: scan.MatchOff}, func(string, error) {})

	p := &Plan{Generation: 2}
	p.Finalize(d, &scan.Stats{Files: 1, Bytes: 30}, nil)

	if p.Summary.DeletedFiles != 2 {
		t.Fatalf("summary counted %d deletions, want 2", p.Summary.DeletedFiles)
	}
	got := map[string]bool{}
	for _, path := range p.Deleted {
		got[path] = true
	}
	if !got["/storage/gone-a"] || !got["/storage/gone-b"] {
		t.Errorf("plan.Deleted = %v, want both deleted paths", p.Deleted)
	}
	if got["/storage/stays"] {
		t.Error("a path that is still on disk was listed as deleted")
	}
}

// The plan file is what the writer reads back, so the paths have to survive
// JSON as well as Finalize.
func TestDeletedPathsSurviveThePlanFile(t *testing.T) {
	p := &Plan{Format: Format, Generation: 2, Deleted: []string{"/storage/gone"}}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var back Plan
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Deleted) != 1 || back.Deleted[0] != "/storage/gone" {
		t.Errorf("Deleted = %v after a round trip through the plan file", back.Deleted)
	}
}
