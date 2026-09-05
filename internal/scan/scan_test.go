package scan

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"kura/internal/catalog"
)

func noErr(string, error) {}

func catalogOf(recs ...*catalog.Record) *catalog.Catalog {
	c := catalog.New()
	c.Add(recs...)
	return c
}

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

func pathsOf(files []*File) []string {
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

func TestDedupeRoots(t *testing.T) {
	cases := []struct {
		name        string
		in          []string
		wantKept    []string
		wantDropped int
	}{
		{
			name:     "disjoint roots are all kept",
			in:       []string{"/storage", "/storage_ssd"},
			wantKept: []string{"/storage", "/storage_ssd"},
		},
		{
			name:        "a child root is dropped",
			in:          []string{"/storage", "/storage/Music"},
			wantKept:    []string{"/storage"},
			wantDropped: 1,
		},
		{
			name:        "order does not matter",
			in:          []string{"/storage/Music", "/storage"},
			wantKept:    []string{"/storage"},
			wantDropped: 1,
		},
		{
			name:        "exact duplicates collapse",
			in:          []string{"/storage", "/storage"},
			wantKept:    []string{"/storage"},
			wantDropped: 1,
		},
		{
			name:        "trailing slashes are normalised",
			in:          []string{"/storage/", "/storage"},
			wantKept:    []string{"/storage"},
			wantDropped: 1,
		},
		{
			// A sibling sharing a name prefix must not be mistaken for a child.
			name:     "prefix siblings are not nested",
			in:       []string{"/storage", "/storage2"},
			wantKept: []string{"/storage", "/storage2"},
		},
		{
			name:        "deeply nested roots collapse to the shallowest",
			in:          []string{"/storage/a/b/c", "/storage/a", "/storage"},
			wantKept:    []string{"/storage"},
			wantDropped: 2,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := make([]Root, len(c.in))
			for i, p := range c.in {
				in[i] = Root{Scan: p}
			}
			kept, dropped := dedupeRoots(in)
			got := make([]string, len(kept))
			for i, r := range kept {
				got[i] = r.Scan
			}
			if !slices.Equal(got, c.wantKept) {
				t.Errorf("kept = %v, want %v", got, c.wantKept)
			}
			if len(dropped) != c.wantDropped {
				t.Errorf("dropped %v, want %d entries", dropped, c.wantDropped)
			}
		})
	}
}

// TestWalkOverlappingRootsCountsOnce is the bug this guards: scanning a parent
// and its child together used to walk the child twice, which would write the
// same files to two volumes and duplicate their catalog entries.
func TestWalkOverlappingRootsCountsOnce(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]int{
		"Music/a.flac":   1000,
		"Music/b.flac":   2000,
		"Movie/film.mkv": 4000,
	})
	ex := NewExcluder(nil, true)

	only, _, err := Walk([]Root{{Scan: root}}, ex, noErr)
	if err != nil {
		t.Fatal(err)
	}
	both, stats, err := Walk([]Root{{Scan: root}, {Scan: filepath.Join(root, "Music")}}, ex, noErr)
	if err != nil {
		t.Fatal(err)
	}

	if len(both) != len(only) {
		t.Errorf("overlapping roots produced %d files, the parent alone gives %d", len(both), len(only))
	}
	if len(stats.SkippedRoots) != 1 {
		t.Errorf("expected the nested root to be reported as skipped, got %v", stats.SkippedRoots)
	}

	seen := make(map[string]bool)
	for _, f := range both {
		if seen[f.Path] {
			t.Errorf("%s was scanned more than once", f.Path)
		}
		seen[f.Path] = true
	}
	if stats.Bytes != 7000 {
		t.Errorf("scanned %d bytes, want 7000 counted once", stats.Bytes)
	}
}

func TestWalkSymlinkedRootsCollapse(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	writeTree(t, real, map[string]int{"x.bin": 500})
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	files, stats, err := Walk([]Root{{Scan: real}, {Scan: link}}, NewExcluder(nil, true), noErr)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Errorf("two names for one directory should scan once, got %d files", len(files))
	}
	if len(stats.SkippedRoots) != 1 {
		t.Errorf("expected the symlinked duplicate to be reported, got %v", stats.SkippedRoots)
	}
}

func TestWalkRecordsSymlinksWithoutFollowing(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]int{"real/file.bin": 300})
	// A symlink pointing back at its own tree would loop a following walk.
	if err := os.Symlink(root, filepath.Join(root, "loop")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	files, stats, err := Walk([]Root{{Scan: root}}, NewExcluder(nil, true), noErr)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Symlinks != 1 {
		t.Errorf("expected 1 symlink recorded, got %d", stats.Symlinks)
	}
	if stats.Files != 1 || stats.Bytes != 300 {
		t.Errorf("expected exactly one regular file of 300 bytes, got %d files / %d bytes", stats.Files, stats.Bytes)
	}
	var link *File
	for _, f := range files {
		if filepath.Base(f.Path) == "loop" {
			link = f
		}
	}
	if link == nil || link.Link != root {
		t.Errorf("symlink target not recorded: %+v", link)
	}
	if link.IsRegular() {
		t.Error("a symlink must not be treated as data to write")
	}
}

func TestWalkDetectsHardlinks(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]int{"orig.bin": 4096})
	if err := os.Link(filepath.Join(root, "orig.bin"), filepath.Join(root, "dup.bin")); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}

	files, stats, err := Walk([]Root{{Scan: root}}, NewExcluder(nil, true), noErr)
	if err != nil {
		t.Fatal(err)
	}
	if stats.HardLinks != 1 {
		t.Errorf("expected 1 hardlinked duplicate, got %d", stats.HardLinks)
	}
	if stats.Bytes != 4096 {
		t.Errorf("hardlinked data must be counted once: got %d bytes, want 4096", stats.Bytes)
	}

	var payload int64
	for _, f := range files {
		if f.IsRegular() {
			payload += f.Size
		}
	}
	if payload != 4096 {
		t.Errorf("write set is %d bytes, want 4096 stored once", payload)
	}
}

func TestExcluder(t *testing.T) {
	ex := NewExcluder([]string{
		"/storage/Archive",
		"/storage/Events & Performances",
		"/storage/*/cache",
		"lost+found",
		"*.partial",
		"# a comment",
		"",
	}, true)

	excluded := []string{
		"/storage/Archive",
		"/storage/Archive/deep/file.mkv",
		"/storage/Events & Performances/show.mkv",
		"/storage/Music/cache",
		"/storage/lost+found",
		"/storage/x/y/download.partial",
		"/storage/code/.git",
	}
	for _, p := range excluded {
		if !ex.Match(p) {
			t.Errorf("%s should be excluded", p)
		}
	}

	kept := []string{
		"/storage/Archived/file.mkv", // prefix must stop at a path separator
		"/storage/Music/album.flac",
		"/storage/cache", // the glob requires an intermediate component
		"/storage/x.partial.mkv",
	}
	for _, p := range kept {
		if ex.Match(p) {
			t.Errorf("%s should not be excluded", p)
		}
	}
}

func TestParseRoot(t *testing.T) {
	cases := []struct {
		in       string
		wantScan string
		wantAs   string
		wantErr  bool
	}{
		{in: "/storage", wantScan: "/storage"},
		{in: "/storage/", wantScan: "/storage"},
		{in: "/storage/.zfs/snapshot/b1=/storage", wantScan: "/storage/.zfs/snapshot/b1", wantAs: "/storage"},
		{in: " /a = /b ", wantScan: "/a", wantAs: "/b"},
		{in: "", wantErr: true},
		{in: "/storage=", wantErr: true},
	}
	for _, c := range cases {
		got, err := ParseRoot(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseRoot(%q) should have failed", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseRoot(%q): %v", c.in, err)
			continue
		}
		if got.Scan != c.wantScan || got.As != c.wantAs {
			t.Errorf("ParseRoot(%q) = {%q, %q}, want {%q, %q}", c.in, got.Scan, got.As, c.wantScan, c.wantAs)
		}
	}
}

func TestCanonicalRoots(t *testing.T) {
	got := CanonicalRoots([]Root{
		{Scan: "/storage/.zfs/snapshot/x", As: "/storage"},
		{Scan: "/storage_ssd"},
	})
	want := []string{"/storage", "/storage_ssd"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("CanonicalRoots[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestSnapshotPathsAreRecordedCanonically is what makes a snapshot-based backup
// viable at all. A ZFS snapshot lives under .zfs/snapshot/<label> and the label
// changes every cycle; scanning it as-is would give every file a new path, so
// every cycle would look like a first backup and rewrite the whole archive.
func TestSnapshotPathsAreRecordedCanonically(t *testing.T) {
	base := t.TempDir()
	live := filepath.Join(base, "storage")
	snap := filepath.Join(base, "storage/.zfs/snapshot/auto-2026-09-02")
	writeTree(t, snap, map[string]int{
		"Movie/film.mkv": 4096,
		"Docs/spec.md":   1024,
	})

	files, _, err := Walk([]Root{{Scan: snap, As: live}}, NewExcluder(nil, true), noErr)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}

	for _, f := range files {
		if !strings.HasPrefix(f.Path, live+"/") {
			t.Errorf("path %q should be recorded under the live root %q", f.Path, live)
		}
		if strings.Contains(f.Path, ".zfs") {
			t.Errorf("the snapshot label leaked into the catalog identity: %q", f.Path)
		}
		if !strings.HasPrefix(f.Real, snap+"/") {
			t.Errorf("Real %q should point into the snapshot for reading", f.Real)
		}
		if f.OpenPath() != f.Real {
			t.Errorf("OpenPath should read from the snapshot, got %q", f.OpenPath())
		}
		// The rewritten path must still resolve to a readable file.
		if _, err := os.Stat(f.OpenPath()); err != nil {
			t.Errorf("OpenPath is not readable: %v", err)
		}
	}
}

// TestTwoSnapshotsInARowAreNotAllNew is the payoff: consecutive cycles read
// from differently-named snapshots and still see an unchanged tree.
func TestTwoSnapshotsInARowAreNotAllNew(t *testing.T) {
	base := t.TempDir()
	live := filepath.Join(base, "storage")
	spec := map[string]int{
		"Movie/film.mkv": 4096,
		"Docs/spec.md":   1024,
	}

	scanSnapshot := func(label string) []*File {
		snap := filepath.Join(base, "storage/.zfs/snapshot", label)
		writeTree(t, snap, spec)
		files, _, err := Walk([]Root{{Scan: snap, As: live}}, NewExcluder(nil, true), noErr)
		if err != nil {
			t.Fatal(err)
		}
		return files
	}

	first := scanSnapshot("auto-2026-07-01")

	// Stand in for the writer: record what the first cycle put on media.
	var recs []*catalog.Record
	for _, f := range first {
		h, err := SampleHash(f.OpenPath(), f.Size)
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, &catalog.Record{
			Path: f.Path, Size: f.Size, ModTime: f.ModTime, CTime: f.CTime, Inode: f.Ino,
			Hash: h, HashKind: catalog.HashKindSample, Volume: "LTO001", Gen: 1,
		})
	}
	cat := catalogOf(recs...)

	// A different snapshot of identical content, as the next cycle would see.
	second := scanSnapshot("auto-2026-09-02")
	d := Diff(second, cat, Options{Match: MatchFull, Jobs: 2, IgnoreInode: true}, noErr)

	if len(d.New) != 0 {
		t.Errorf("a new snapshot of unchanged content produced %d new files: %v", len(d.New), pathsOf(d.New))
	}
	if len(d.Deleted) != 0 {
		t.Errorf("nothing should look deleted, got %v", recPathsOf(d.Deleted))
	}
	if d.WriteBytes() != 0 {
		t.Errorf("an unchanged tree must write nothing, got %d bytes", d.WriteBytes())
	}
	// The files were recreated, so ctime moved and each one had to be verified
	// rather than blindly rewritten. That is the suspect path doing its job.
	if d.UnchangedFiles+d.CaughtByCTime != 2 {
		t.Errorf("expected both files accounted for, got %d unchanged / %d caught",
			d.UnchangedFiles, d.CaughtByCTime)
	}
}

// TestSnapshotExclusionsMatchCanonicalPaths keeps one exclude file working
// regardless of which snapshot is being read.
func TestSnapshotExclusionsMatchCanonicalPaths(t *testing.T) {
	base := t.TempDir()
	live := filepath.Join(base, "storage")
	snap := filepath.Join(base, "storage/.zfs/snapshot/auto-1")
	writeTree(t, snap, map[string]int{
		"Movie/film.mkv":   4096,
		"Archive/old.tar":  8192,
		"Archive/deep/x.z": 512,
	})

	// Written against the live path, as an operator would.
	ex := NewExcluder([]string{live + "/Archive"}, true)
	files, stats, err := Walk([]Root{{Scan: snap, As: live}}, ex, noErr)
	if err != nil {
		t.Fatal(err)
	}

	if len(files) != 1 || filepath.Base(files[0].Path) != "film.mkv" {
		t.Errorf("expected only film.mkv to survive exclusion, got %v", pathsOf(files))
	}
	if stats.Excluded == 0 {
		t.Error("the Archive subtree should have been excluded")
	}
}
