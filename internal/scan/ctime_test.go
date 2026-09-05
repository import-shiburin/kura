package scan

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"kura/internal/catalog"
)

// rewriteKeepingMtime rewrites a file's contents and puts its mtime back, which
// is what package managers, archive extraction, rsync -t and torrent clients
// routinely do. Size is preserved so that check cannot save us either.
func rewriteKeepingMtime(t *testing.T, path string, content []byte) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := info.ModTime()
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func statOf(t *testing.T, path string) *File {
	t.Helper()
	files, _, err := Walk([]Root{{Scan: filepath.Dir(path)}}, NewExcluder(nil, true), noErr)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("%s not found in scan", path)
	return nil
}

// TestCTimeCatchesContentChangeUnderRestoredMtime is the failure an mtime-only
// check cannot see: the file really changed, but size and mtime are identical,
// so it would be called unchanged and never backed up again. The loss is only
// discovered at restore time.
func TestCTimeCatchesContentChangeUnderRestoredMtime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.bin")
	original := make([]byte, 4096)
	for i := range original {
		original[i] = byte(i % 251)
	}
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}

	before := statOf(t, path)
	fullBefore, err := FullHash(path)
	if err != nil {
		t.Fatal(err)
	}
	sampledBefore, err := SampleHash(path, before.Size)
	if err != nil {
		t.Fatal(err)
	}
	rec := &catalog.Record{
		Path: path, Size: before.Size, ModTime: before.ModTime,
		CTime: before.CTime, Inode: before.Ino,
		Hash: sampledBefore, HashKind: catalog.HashKindSample, FullHash: fullBefore,
		Volume: "LTO001", Gen: 1,
	}

	// Same length, different bytes, mtime restored.
	time.Sleep(10 * time.Millisecond)
	tampered := make([]byte, len(original))
	copy(tampered, original)
	tampered[2000] ^= 0xFF
	rewriteKeepingMtime(t, path, tampered)

	after := statOf(t, path)
	if after.Size != before.Size || after.ModTime != before.ModTime {
		t.Fatalf("test setup failed to preserve size and mtime: %d/%d vs %d/%d",
			after.Size, after.ModTime, before.Size, before.ModTime)
	}
	if after.CTime == before.CTime {
		t.Skip("filesystem did not advance ctime; nothing to detect here")
	}

	// With the corroborating checks on, the change is caught.
	d := Diff([]*File{after}, catalogOf(rec), Options{Match: MatchFull, Jobs: 1}, noErr)
	if d.Suspect != 1 {
		t.Errorf("expected the ctime mismatch to flag the file, got %d suspects", d.Suspect)
	}
	if d.CaughtByCTime != 1 {
		t.Errorf("expected the rewrite to be caught, got %d", d.CaughtByCTime)
	}
	if len(d.Modified) != 1 {
		t.Fatalf("the tampered file must be rewritten, got %d modified", len(d.Modified))
	}
	if d.UnchangedFiles != 0 {
		t.Error("the tampered file must not count as unchanged")
	}

	// And with them off, it is silently missed. This is the bug being guarded.
	loose := Diff([]*File{after}, catalogOf(rec),
		Options{Match: MatchFull, Jobs: 1, IgnoreCTime: true, IgnoreInode: true}, noErr)
	if loose.UnchangedFiles != 1 || len(loose.Modified) != 0 {
		t.Errorf("expected -ignore-ctime to miss the change (documenting the risk), got %d unchanged / %d modified",
			loose.UnchangedFiles, len(loose.Modified))
	}
}

// TestCTimeChangeWithoutContentChangeIsNotRewritten is the other half, and the
// reason a ctime mismatch triggers a read rather than a rewrite. A chmod sweep
// bumps ctime on every file; treating that as "modified" the way a disk-backed
// tool can afford to would rewrite the whole archive to media.
func TestCTimeChangeWithoutContentChangeIsNotRewritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "untouched.bin")
	payload := make([]byte, 8192)
	for i := range payload {
		payload[i] = byte(i % 97)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	before := statOf(t, path)
	full, err := FullHash(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := &catalog.Record{
		Path: path, Size: before.Size, ModTime: before.ModTime,
		CTime: before.CTime, Inode: before.Ino,
		FullHash: full, Hash: "x", HashKind: catalog.HashKindSample,
		Volume: "LTO001", Gen: 1,
	}

	time.Sleep(10 * time.Millisecond)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	after := statOf(t, path)
	if after.CTime == before.CTime {
		t.Skip("chmod did not advance ctime here")
	}

	d := Diff([]*File{after}, catalogOf(rec), Options{Match: MatchFull, Jobs: 1}, noErr)

	if d.Suspect != 1 {
		t.Errorf("the chmod should have made the file suspect, got %d", d.Suspect)
	}
	if d.CaughtByCTime != 0 {
		t.Errorf("content was identical; nothing should have been caught as changed, got %d", d.CaughtByCTime)
	}
	if len(d.Modified) != 0 {
		t.Errorf("a chmod must not cause a rewrite to media, got %d modified", len(d.Modified))
	}
	if d.UnchangedFiles != 1 {
		t.Errorf("expected the file to settle as unchanged, got %d", d.UnchangedFiles)
	}
	if len(d.Refreshed) != 1 {
		t.Errorf("the catalog entry needs its metadata refreshed, got %d", len(d.Refreshed))
	}
	if d.WriteBytes() != 0 {
		t.Errorf("a chmod should write nothing to media, got %d bytes", d.WriteBytes())
	}
}

// TestUnverifiableSuspectIsRewritten covers the conservative fallback: a record
// with no stored digest cannot be checked, so the safe answer is to write it.
func TestUnverifiableSuspectIsRewritten(t *testing.T) {
	rec := &catalog.Record{
		Path: "/storage/x", Size: 100, ModTime: 5, CTime: 10, Inode: 42,
		Volume: "LTO001", Gen: 1, // no Hash, no FullHash
	}
	f := &File{Path: "/storage/x", Size: 100, ModTime: 5, CTime: 99, Ino: 42, Mode: 0o644}

	d := Diff([]*File{f}, catalogOf(rec), Options{Match: MatchFull, Jobs: 1}, noErr)

	if d.Unverifiable != 1 {
		t.Errorf("expected 1 unverifiable suspect, got %d", d.Unverifiable)
	}
	if len(d.Modified) != 1 {
		t.Errorf("an unverifiable suspect must be rewritten, got %d modified", len(d.Modified))
	}
}

// TestOldCatalogsDegradeToMtime keeps catalogs written before these fields
// existed usable: a zero ctime means "unknown", not "mismatch".
func TestOldCatalogsDegradeToMtime(t *testing.T) {
	rec := &catalog.Record{Path: "/storage/x", Size: 100, ModTime: 5, Volume: "LTO001", Gen: 1} // no ctime/inode
	f := &File{Path: "/storage/x", Size: 100, ModTime: 5, CTime: 12345, Ino: 42, Mode: 0o644}

	d := Diff([]*File{f}, catalogOf(rec), Options{Match: MatchFull, Jobs: 1}, noErr)

	if d.Suspect != 0 {
		t.Errorf("a record with no recorded ctime must not be suspect, got %d", d.Suspect)
	}
	if d.UnchangedFiles != 1 {
		t.Errorf("expected the file to settle as unchanged on mtime alone, got %d", d.UnchangedFiles)
	}
}

func TestWalkRecordsCTime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := statOf(t, path)
	if f.CTime == 0 {
		t.Error("ctime was not captured during the scan")
	}
	if f.Ino == 0 {
		t.Error("inode was not captured during the scan")
	}
}
