package scan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kura/internal/catalog"
)

// A scan that cannot read something has to say "unknown", never "absent". The
// difference is the whole archive: every live path the walk did not reach is
// reported as deleted, and the next write tombstones it without needing a
// cartridge. These three are the ways a walk fails to reach a tree.

// TestWalkFailsOnARootThatCannotBeRead: filepath.WalkDir calls back exactly
// once for a root it cannot stat, with a nil entry, and Walk answers that
// callback with "skip". A mistyped -root, or a snapshot path that is not
// mounted this morning, scans as zero files with one warning and exit 0.
func TestWalkFailsOnARootThatCannotBeRead(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-here")
	files, stats, err := Walk([]Root{{Scan: root}}, NewExcluder(nil, true), noErr)
	if err == nil {
		t.Fatalf("a root that does not exist was walked as %d files with %d errors; every path under it would be tombstoned at the next write",
			len(files), stats.Errors)
	}
}

// TestWalkFollowsASymlinkedRoot: WalkDir never follows symlinks, the root
// included, so a -root that is itself a link is recorded as one symlink and
// the tree behind it is never entered. /storage being a link to a dataset
// mount is an ordinary NAS arrangement.
func TestWalkFollowsASymlinkedRoot(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	writeTree(t, real, map[string]int{"a.bin": 100, "sub/b.bin": 200})
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	_, stats, err := Walk([]Root{{Scan: link}}, NewExcluder(nil, true), noErr)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files != 2 {
		t.Fatalf("a root reached through a symlink yielded %d files, want 2: the link itself was recorded and the tree behind it was never entered",
			stats.Files)
	}
}

// TestAnUnreadableSubtreeIsNotADeletion: a directory the walk could not enter
// is skipped and counted, and Diff then reports everything the catalog holds
// under it as deleted. Refusing the scan outright is one right answer;
// excluding the unreadable subtree from deletion detection is the other.
func TestAnUnreadableSubtreeIsNotADeletion(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything; the permission failure this test needs cannot be produced")
	}
	root := t.TempDir()
	writeTree(t, root, map[string]int{"a/x.bin": 100, "b/y.bin": 100})

	// The catalog knows both, from an earlier cycle.
	files, _, err := Walk([]Root{{Scan: root}}, NewExcluder(nil, true), noErr)
	if err != nil {
		t.Fatal(err)
	}
	var recs []*catalog.Record
	for _, f := range files {
		recs = append(recs, &catalog.Record{
			Path: f.Path, Size: f.Size, ModTime: f.ModTime, CTime: f.CTime, Inode: f.Ino,
			Volume: "V001", Gen: 1,
		})
	}
	cat := catalogOf(recs...)

	locked := filepath.Join(root, "b")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })

	var failed []string
	files, _, err = Walk([]Root{{Scan: root}}, NewExcluder(nil, true), func(p string, _ error) { failed = append(failed, p) })
	if err != nil {
		return // the scan refused, which is a correct answer
	}
	if len(failed) == 0 {
		t.Fatal("the unreadable directory did not produce a walk error; the test cannot run")
	}

	d := Diff(files, cat, Options{Match: MatchOff}, noErr)
	for _, rec := range d.Deleted {
		if strings.HasPrefix(rec.Path, locked+"/") {
			t.Errorf("%s was reported deleted because its directory could not be read; an unreadable subtree is unknown, not absent", rec.Path)
		}
	}
}
