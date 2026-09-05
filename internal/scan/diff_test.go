package scan

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"kura/internal/catalog"
)

func TestDiffClassification(t *testing.T) {
	cat := catalogOf(
		&catalog.Record{Path: "/storage/keep", Size: 100, ModTime: 1000, Volume: "LTO001", Gen: 1},
		&catalog.Record{Path: "/storage/touched", Size: 200, ModTime: 1000, Volume: "LTO001", Gen: 1},
		&catalog.Record{Path: "/storage/gone", Size: 300, ModTime: 1000, Volume: "LTO001", Gen: 1},
	)
	files := []*File{
		{Path: "/storage/keep", Size: 100, ModTime: 1000, Mode: 0o644},
		{Path: "/storage/touched", Size: 250, ModTime: 2000, Mode: 0o644},
		{Path: "/storage/fresh", Size: 400, ModTime: 2000, Mode: 0o644},
	}

	d := Diff(files, cat, Options{Match: MatchOff, Jobs: 1}, noErr)

	if d.UnchangedFiles != 1 || d.UnchangedBytes != 100 {
		t.Errorf("unchanged: got %d files / %d bytes, want 1 / 100", d.UnchangedFiles, d.UnchangedBytes)
	}
	if len(d.Modified) != 1 || d.Modified[0].Path != "/storage/touched" {
		t.Errorf("modified: got %v", pathsOf(d.Modified))
	}
	if len(d.New) != 1 || d.New[0].Path != "/storage/fresh" {
		t.Errorf("new: got %v", pathsOf(d.New))
	}
	if len(d.Deleted) != 1 || d.Deleted[0].Path != "/storage/gone" {
		t.Errorf("deleted: got %v", recPathsOf(d.Deleted))
	}
	if got, want := d.WriteBytes(), int64(650); got != want {
		t.Errorf("write bytes %d, want %d", got, want)
	}
}

// TestDiffReusesMovedContent covers the case that saves the most media in a
// library that gets reorganised: identical content appearing under a new path.
func TestDiffReusesMovedContent(t *testing.T) {
	dir := t.TempDir()
	moved := filepath.Join(dir, "Season 01", "ep01.mkv")
	if err := os.MkdirAll(filepath.Dir(moved), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 4096)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	if err := os.WriteFile(moved, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	hash, err := SampleHash(moved, int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}

	cat := catalogOf(&catalog.Record{
		Path: filepath.Join(dir, "ep01.mkv"), Size: int64(len(payload)), ModTime: 1000,
		Hash: hash, HashKind: catalog.HashKindSample, Volume: "LTO003", Gen: 2,
	})
	files := []*File{{Path: moved, Size: int64(len(payload)), ModTime: 5000, Mode: 0o644}}

	d := Diff(files, cat, Options{Match: MatchSample, Jobs: 2}, noErr)

	if len(d.Reused) != 1 {
		t.Fatalf("expected 1 reuse, got %d (new=%d deleted=%d)", len(d.Reused), len(d.New), len(d.Deleted))
	}
	if d.Reused[0].To.Path != moved {
		t.Errorf("reuse target %s, want %s", d.Reused[0].To.Path, moved)
	}
	if d.Reused[0].From.Volume != "LTO003" {
		t.Errorf("reuse should point at the volume already holding the data, got %q", d.Reused[0].From.Volume)
	}
	if d.WriteBytes() != 0 {
		t.Errorf("a move should write nothing, got %d bytes", d.WriteBytes())
	}

	// The old path really is gone from disk, so it gets tombstoned. Both facts
	// are recorded: the path stopped existing at this generation, and the bytes
	// live on under a new name. That pairing is what lets a restore of an
	// earlier generation still find the file under its original path.
	if len(d.Deleted) != 1 {
		t.Fatalf("the vacated path should be tombstoned, got %d deletions", len(d.Deleted))
	}
	if d.Deleted[0].Path == moved {
		t.Error("the tombstone should be for the old path, not the new one")
	}
}

// TestDiffHashesOnlySizeCollisions is what keeps a routine cycle from reading
// the whole archive: a new file is only opened when some vanished record has
// exactly its size.
func TestDiffHashesOnlySizeCollisions(t *testing.T) {
	cat := catalogOf(
		&catalog.Record{Path: "/storage/old", Size: 100, ModTime: 1, Hash: "deadbeef", HashKind: catalog.HashKindSample, Volume: "LTO001"},
	)
	files := []*File{
		{Path: "/storage/same-size", Size: 100, ModTime: 2, Mode: 0o644}, // candidate, will be read
		{Path: "/storage/other", Size: 999, ModTime: 2, Mode: 0o644},     // no collision, never read
	}

	var opened []string
	d := Diff(files, cat, Options{Match: MatchSample, Jobs: 1}, func(p string, _ error) { opened = append(opened, p) })

	if d.HashedFiles != 1 {
		t.Errorf("expected exactly 1 file considered for hashing, got %d", d.HashedFiles)
	}
	// Neither file exists on disk, so the one that was tried reports an error
	// and falls back to "new"; the one with no size collision is never opened.
	if len(opened) != 1 || opened[0] != "/storage/same-size" {
		t.Errorf("expected only the size-colliding file to be opened, got %v", opened)
	}
	if len(d.New) != 2 {
		t.Errorf("both files should end up as new, got %d", len(d.New))
	}
}

func TestDiffSkipsNonRegularFiles(t *testing.T) {
	files := []*File{
		{Path: "/storage/link", Size: 0, Mode: os.ModeSymlink, Link: "/elsewhere"},
		{Path: "/storage/dup", Size: 500, Mode: 0o644, HardLinkTo: "/storage/orig"},
		{Path: "/storage/orig", Size: 500, Mode: 0o644},
	}
	d := Diff(files, catalogOf(), Options{Match: MatchOff, Jobs: 1}, noErr)

	if len(d.Metadata) != 2 {
		t.Errorf("symlink and hardlink should be catalog-only, got %d metadata entries", len(d.Metadata))
	}
	if d.WriteBytes() != 500 {
		t.Errorf("hardlinked data should be stored once: got %d bytes, want 500", d.WriteBytes())
	}
}

// TestPastDeletionsAreNotReReported is the bug tombstones would otherwise
// introduce: a file removed generations ago must not show up as newly deleted
// on every subsequent cycle.
func TestPastDeletionsAreNotReReported(t *testing.T) {
	cat := catalogOf(
		&catalog.Record{Path: "/storage/a", Size: 10, Volume: "LTO001", Gen: 1, DeletedGen: 3},
		&catalog.Record{Path: "/storage/b", Size: 20, Volume: "LTO001", Gen: 1},
		&catalog.Record{Path: "/storage/b", Size: 25, Volume: "LTO002", Gen: 2},
		&catalog.Record{Path: "/storage/c", Size: 30, Volume: "LTO001", Gen: 1, DeletedGen: 2},
		&catalog.Record{Path: "/storage/d", Size: 40, Volume: "LTO003", Gen: 3},
	)
	files := []*File{
		{Path: "/storage/b", Size: 25, ModTime: 0, Mode: 0o644},
		{Path: "/storage/d", Size: 40, ModTime: 0, Mode: 0o644},
	}

	d := Diff(files, cat, Options{Match: MatchOff, Jobs: 1}, noErr)
	if len(d.Deleted) != 0 {
		t.Errorf("already-tombstoned paths were re-reported as deleted: %v", recPathsOf(d.Deleted))
	}
	if d.UnchangedFiles != 2 {
		t.Errorf("expected both live files unchanged, got %d", d.UnchangedFiles)
	}
}

// TestReAddedContentCostsNothing is the direct consequence of never reclaiming:
// content deleted generations ago is still on its volume, so putting it back is
// a catalog entry rather than a write.
func TestReAddedContentCostsNothing(t *testing.T) {
	dir := t.TempDir()
	payload := bytes.Repeat([]byte("re-added"), 512)
	path := filepath.Join(dir, "returned.bin")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := SampleHash(path, int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}

	// The record was tombstoned two generations ago; its bytes are still there.
	cat := catalogOf(&catalog.Record{
		Path: filepath.Join(dir, "gone-long-ago.bin"), Size: int64(len(payload)),
		Hash: h, HashKind: catalog.HashKindSample, Volume: "LTO004", Gen: 1, DeletedGen: 2,
	})
	files := []*File{{Path: path, Size: int64(len(payload)), ModTime: 900, Mode: 0o644}}

	d := Diff(files, cat, Options{Match: MatchSample, Jobs: 1}, noErr)

	if len(d.Reused) != 1 {
		t.Fatalf("re-added content should match the tombstoned copy, got %d reuses", len(d.Reused))
	}
	if d.Reused[0].From.Volume != "LTO004" {
		t.Errorf("reuse should point at LTO004, got %q", d.Reused[0].From.Volume)
	}
	if d.WriteBytes() != 0 {
		t.Errorf("re-adding retained content should write nothing, got %d bytes", d.WriteBytes())
	}
}

// TestFullHashRejectsSampledCollision guards the one way a match can go wrong:
// two files sharing a size and both end megabytes but differing in the middle.
// Accepting that on the sampled digest alone would restore the wrong bytes.
func TestFullHashRejectsSampledCollision(t *testing.T) {
	dir := t.TempDir()
	size := 3 * sampleWindow

	mk := func(name string, middle byte) string {
		buf := make([]byte, size)
		for i := range buf {
			buf[i] = byte(i % 251)
		}
		// Differ only in the middle, which the sampled digest never reads.
		for i := sampleWindow + 100; i < sampleWindow+200; i++ {
			buf[i] = middle
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, buf, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	onMedia := mk("original.bin", 0xAA)
	onDisk := mk("lookalike.bin", 0xBB)

	sampledA, _ := SampleHash(onMedia, int64(size))
	sampledB, _ := SampleHash(onDisk, int64(size))
	if sampledA != sampledB {
		t.Skip("sampled digests did not collide; nothing to guard here")
	}
	fullA, err := FullHash(onMedia)
	if err != nil {
		t.Fatal(err)
	}

	rec := &catalog.Record{
		Path: onMedia, Size: int64(size), Hash: sampledA, HashKind: catalog.HashKindSample,
		FullHash: fullA, Volume: "LTO001", Gen: 1, DeletedGen: 2,
	}
	files := []*File{{Path: onDisk, Size: int64(size), ModTime: 5, Mode: 0o644}}

	// Sampled matching is fooled...
	loose := Diff(files, catalogOf(rec), Options{Match: MatchSample, Jobs: 1}, noErr)
	if len(loose.Reused) != 1 {
		t.Fatalf("the sampled digests collide, so MatchSample should have matched; got %d", len(loose.Reused))
	}

	// ...and the full digest catches it.
	strict := Diff(files, catalogOf(rec), Options{Match: MatchFull, Jobs: 1}, noErr)
	if len(strict.Reused) != 0 {
		t.Error("MatchFull accepted a file whose full digest differs")
	}
	if len(strict.New) != 1 {
		t.Errorf("the disproved candidate should be written as new, got %d new files", len(strict.New))
	}
}

// TestLostVersionIsRewrittenDespiteMatchingStat is DESIGN §5.1 consequence 1,
// and the reason marking something lost is self-healing: the file is untouched
// on disk, so every stat check says "unchanged", but the copy on media is gone
// and the next cycle has to put it back.
func TestLostVersionIsRewrittenDespiteMatchingStat(t *testing.T) {
	rec := &catalog.Record{
		Path: "/storage/x", Size: 100, ModTime: 5, CTime: 10, Inode: 42,
		Volume: "LTO001", Gen: 1,
		Lost: &catalog.Lost{Gen: 4, Reason: catalog.LostMedia, Detail: "LTO001 unreadable"},
	}
	f := &File{Path: "/storage/x", Size: 100, ModTime: 5, CTime: 10, Ino: 42, Mode: 0o644}

	d := Diff([]*File{f}, catalogOf(rec), Options{Match: MatchFull, Jobs: 1}, noErr)

	if d.ReplacingLost != 1 {
		t.Errorf("expected the lost copy to be counted as such, got %d", d.ReplacingLost)
	}
	if len(d.Modified) != 1 || d.Modified[0].Path != "/storage/x" {
		t.Fatalf("a lost version must be written again, got %v", pathsOf(d.Modified))
	}
	if d.UnchangedFiles != 0 {
		t.Error("stat data matches, but the file must not settle as unchanged")
	}
	if d.WriteBytes() != 100 {
		t.Errorf("the whole file has to be re-sourced, got %d bytes", d.WriteBytes())
	}
	// It is counted apart from ordinary edits: this measures damage, not work.
	if d.Suspect != 0 {
		t.Errorf("a lost version needs no verification read, got %d suspects", d.Suspect)
	}
}

// TestLostRecordIsNeverReusedAsASource is the trap the lost marker opens if the
// reuse index ignores it. Reuse writes nothing, so pointing a new path at bytes
// already known to be gone would silently turn one damaged object into two
// unrestorable paths, and no verify pass would ever look at it.
func TestLostRecordIsNeverReusedAsASource(t *testing.T) {
	dir := t.TempDir()
	payload := bytes.Repeat([]byte("content"), 512)
	path := filepath.Join(dir, "returned.bin")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := SampleHash(path, int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	rec := &catalog.Record{
		Path: filepath.Join(dir, "elsewhere.bin"), Size: int64(len(payload)),
		Hash: h, HashKind: catalog.HashKindSample, Volume: "LTO004", Gen: 1,
		Lost: &catalog.Lost{Gen: 3, Reason: catalog.LostMedia, Detail: "LTO004 unreadable"},
	}
	files := []*File{{Path: path, Size: int64(len(payload)), ModTime: 900, Mode: 0o644}}

	d := Diff(files, catalogOf(rec), Options{Match: MatchSample, Jobs: 1}, noErr)

	if len(d.Reused) != 0 {
		t.Errorf("matched against a copy that is gone: %d reuses", len(d.Reused))
	}
	if len(d.New) != 1 {
		t.Fatalf("the content has to be written for real, got %d new files", len(d.New))
	}
	if d.WriteBytes() != int64(len(payload)) {
		t.Errorf("expected the full payload to be written, got %d bytes", d.WriteBytes())
	}
}

// TestReclaimedVersionIsAlsoRewritten covers the other loss reason. Reclamation
// was deliberate, but the current tree still deserves protecting: a file whose
// only copy was on a reclaimed volume has to be written again like any other.
func TestReclaimedVersionIsAlsoRewritten(t *testing.T) {
	rec := &catalog.Record{
		Path: "/storage/x", Size: 100, ModTime: 5, Volume: "LTO001", Gen: 1,
		Lost: &catalog.Lost{Gen: 12, Reason: catalog.LostReclaimed, Detail: "LTO001 reclaimed for capacity"},
	}
	f := &File{Path: "/storage/x", Size: 100, ModTime: 5, Mode: 0o644}

	d := Diff([]*File{f}, catalogOf(rec), Options{Match: MatchFull, Jobs: 1}, noErr)
	if d.ReplacingLost != 1 || len(d.Modified) != 1 {
		t.Errorf("a reclaimed copy still has to be rewritten, got %d lost / %d modified",
			d.ReplacingLost, len(d.Modified))
	}
}

// TestLostVersionOfADeletedPathIsNotRewritten keeps the self-healing rule from
// resurrecting history. The file is gone from disk, so there is nothing to
// re-source; the marker just stands as a record of what was lost.
func TestLostVersionOfADeletedPathIsNotRewritten(t *testing.T) {
	rec := &catalog.Record{
		Path: "/storage/x", Size: 100, ModTime: 5, Volume: "LTO001", Gen: 1, DeletedGen: 2,
		Lost: &catalog.Lost{Gen: 4, Reason: catalog.LostMedia},
	}

	d := Diff(nil, catalogOf(rec), Options{Match: MatchFull, Jobs: 1}, noErr)
	if d.ReplacingLost != 0 || len(d.Modified) != 0 {
		t.Errorf("a path that no longer exists cannot be re-sourced, got %d lost / %d modified",
			d.ReplacingLost, len(d.Modified))
	}
	if len(d.Deleted) != 0 {
		t.Errorf("the path was already tombstoned and must not be re-reported, got %v", recPathsOf(d.Deleted))
	}
}

func TestSampleHashDistinguishesContent(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	if err := os.WriteFile(a, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("hello worlx"), 0o644); err != nil {
		t.Fatal(err)
	}

	ha, err := SampleHash(a, 11)
	if err != nil {
		t.Fatal(err)
	}
	hb, err := SampleHash(b, 11)
	if err != nil {
		t.Fatal(err)
	}
	if ha == hb {
		t.Error("different content produced the same sampled hash")
	}

	again, err := SampleHash(a, 11)
	if err != nil {
		t.Fatal(err)
	}
	if ha != again {
		t.Error("sampled hash is not stable across calls")
	}
}
