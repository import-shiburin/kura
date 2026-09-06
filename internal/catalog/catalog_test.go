package catalog

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func catalogOf(recs ...*Record) *Catalog {
	c := New()
	c.Add(recs...)
	return c
}

// TestAddOrdersVersionsRegardlessOfArrival is what the insertion search in Add
// is for. Every query reads a path's versions as generation-ascending, so a
// record arriving out of order would otherwise make Lookup answer from an older
// version than the one on media.
func TestAddOrdersVersionsRegardlessOfArrival(t *testing.T) {
	c := New()
	c.Add(&Record{Path: "/storage/x", Size: 30, Gen: 3})
	c.Add(&Record{Path: "/storage/x", Size: 10, Gen: 1})
	c.Add(&Record{Path: "/storage/x", Size: 20, Gen: 2})

	got, ok := c.Lookup("/storage/x")
	if !ok || got.Size != 30 {
		t.Errorf("Lookup should return the generation-3 version, got %+v", got)
	}
	for i, want := range []int64{10, 20, 30} {
		if v := c.Versions("/storage/x"); v[i].Size != want {
			t.Errorf("Versions[%d] = %d bytes, want %d", i, v[i].Size, want)
		}
	}
	if r := findPath(c.At(2), "/storage/x"); r == nil || r.Size != 20 {
		t.Errorf("At(2) should give the generation-2 version, got %+v", r)
	}
}

func findPath(recs []*Record, path string) *Record {
	for _, r := range recs {
		if r.Path == path {
			return r
		}
	}
	return nil
}

// history builds a catalog covering three generations of one small tree:
//
//	gen 1: a, b, c written
//	gen 2: b edited (new version on a new volume), c deleted
//	gen 3: d written, a deleted
//
// Nothing is ever removed from the catalog, matching the fact that nothing is
// ever reclaimed from the media.
func history() *Catalog {
	return catalogOf(
		&Record{Path: "/storage/a", Size: 10, Volume: "LTO001", Gen: 1, DeletedGen: 3},
		&Record{Path: "/storage/b", Size: 20, Volume: "LTO001", Gen: 1},
		&Record{Path: "/storage/b", Size: 25, Volume: "LTO002", Gen: 2},
		&Record{Path: "/storage/c", Size: 30, Volume: "LTO001", Gen: 1, DeletedGen: 2},
		&Record{Path: "/storage/d", Size: 40, Volume: "LTO003", Gen: 3},
	)
}

func TestAtReconstructsPastGenerations(t *testing.T) {
	cat := history()

	cases := []struct {
		gen   int
		paths []string
		bytes int64
	}{
		{1, []string{"/storage/a", "/storage/b", "/storage/c"}, 60},
		{2, []string{"/storage/a", "/storage/b"}, 35},
		{3, []string{"/storage/b", "/storage/d"}, 65},
	}

	for _, c := range cases {
		state := cat.At(c.gen)
		var got []string
		var total int64
		for _, r := range state {
			got = append(got, r.Path)
			total += r.Size
		}
		if strings.Join(got, ",") != strings.Join(c.paths, ",") {
			t.Errorf("At(%d) = %v, want %v", c.gen, got, c.paths)
		}
		if total != c.bytes {
			t.Errorf("At(%d) totals %d bytes, want %d", c.gen, total, c.bytes)
		}
	}
}

// TestAtPicksTheVersionOfTheDay is the part that makes this a real
// point-in-time restore rather than just an undelete: an edited file must come
// back as the version that was current then, from the volume that held it.
func TestAtPicksTheVersionOfTheDay(t *testing.T) {
	cat := history()

	older := findPath(cat.At(1), "/storage/b")
	newer := findPath(cat.At(3), "/storage/b")
	if older == nil || newer == nil {
		t.Fatal("/storage/b should exist at both generations")
	}
	if older.Size != 20 || older.Volume != "LTO001" {
		t.Errorf("gen 1 should give the 20-byte version on LTO001, got %d on %s", older.Size, older.Volume)
	}
	if newer.Size != 25 || newer.Volume != "LTO002" {
		t.Errorf("gen 3 should give the 25-byte version on LTO002, got %d on %s", newer.Size, newer.Volume)
	}
}

func TestLookupReturnsOnlyLiveVersions(t *testing.T) {
	cat := history()

	if _, ok := cat.Lookup("/storage/a"); ok {
		t.Error("a deleted path must not look up as live")
	}
	b, ok := cat.Lookup("/storage/b")
	if !ok {
		t.Fatal("/storage/b is still live and should look up")
	}
	if b.Size != 25 {
		t.Errorf("Lookup should return the newest version, got size %d", b.Size)
	}
	if got := len(cat.Versions("/storage/b")); got != 2 {
		t.Errorf("Versions should expose both versions, got %d", got)
	}
	if got := len(cat.LiveRecords()); got != 2 {
		t.Errorf("expected 2 live records (b and d), got %d", got)
	}
}

// TestDeletionAfterRewriteStaysDeleted is the case that breaks if liveness is
// read off individual records instead of the newest version of a path. A file
// written, later rewritten, then deleted leaves an untombstoned first version
// behind; treating that as live would resurrect the path.
func TestDeletionAfterRewriteStaysDeleted(t *testing.T) {
	cat := catalogOf(
		&Record{Path: "/storage/x", Size: 10, Volume: "LTO001", Gen: 1},
		&Record{Path: "/storage/x", Size: 15, Volume: "LTO002", Gen: 3, DeletedGen: 4},
	)

	if _, ok := cat.Lookup("/storage/x"); ok {
		t.Error("the path was deleted at generation 4 and must not look up as live")
	}
	if got := len(cat.LiveRecords()); got != 0 {
		t.Errorf("expected no live records, got %d", got)
	}

	for _, g := range []int{1, 2, 3} {
		if findPath(cat.At(g), "/storage/x") == nil {
			t.Errorf("At(%d): the path existed then and should be restorable", g)
		}
	}
	for _, g := range []int{4, 5} {
		if r := findPath(cat.At(g), "/storage/x"); r != nil {
			t.Errorf("At(%d): the path was gone, but got size %d from %s", g, r.Size, r.Volume)
		}
	}
	// The version of the day, not merely the latest surviving one.
	if r := findPath(cat.At(2), "/storage/x"); r == nil || r.Size != 10 || r.Volume != "LTO001" {
		t.Errorf("At(2) should give the 10-byte version on LTO001, got %+v", r)
	}
	if r := findPath(cat.At(3), "/storage/x"); r == nil || r.Size != 15 || r.Volume != "LTO002" {
		t.Errorf("At(3) should give the 15-byte version on LTO002, got %+v", r)
	}
}

// TestLostVersionIsFlaggedNotOmitted is DESIGN §5.1 consequences 2 and 3. A
// damaged volume must not make a path quietly disappear from a point-in-time
// query — that is the difference between "1,247 objects unavailable" and a
// short tree that looks complete.
func TestLostVersionIsFlaggedNotOmitted(t *testing.T) {
	cat := history()
	b := findPath(cat.At(1), "/storage/b")
	b.MarkLost(Lost{Gen: 9, Reason: LostMedia, Detail: "LTO001 unreadable during verify"})

	got := findPath(cat.At(1), "/storage/b")
	if got == nil {
		t.Fatal("a lost version must still be returned by At, flagged")
	}
	if !got.IsLost() {
		t.Error("the returned record should carry its loss marker")
	}
	// It still knows what it was, which is what lets the content be recognised
	// if it ever turns up again.
	if got.Size != 20 || got.Volume != "LTO001" {
		t.Errorf("a lost record must keep its size and volume, got %d on %s", got.Size, got.Volume)
	}

	// Lookup and LiveRecords likewise: the path exists, it just cannot be
	// produced. Diff needs to see the record in order to rewrite it.
	live := findPath(cat.LiveRecords(), "/storage/b")
	if live == nil {
		t.Error("a lost version must still appear in LiveRecords, or Diff can never re-protect it")
	}
}

// TestLossIsPerVersionNotPerPath is why the marker sits on the record. One
// damaged volume must not condemn a path that a later generation rewrote
// somewhere else.
func TestLossIsPerVersionNotPerPath(t *testing.T) {
	cat := history()
	for _, r := range cat.OnVolume("LTO001") {
		r.MarkLost(Lost{Gen: 9, Reason: LostMedia, Detail: "LTO001 unreadable"})
	}

	if r := findPath(cat.At(1), "/storage/b"); r == nil || !r.IsLost() {
		t.Error("/storage/b at generation 1 was on the damaged volume and should be lost")
	}
	if r := findPath(cat.At(3), "/storage/b"); r == nil || r.IsLost() {
		t.Error("/storage/b was rewritten to LTO002 at generation 2 and is fine from there on")
	}
	if r := findPath(cat.At(3), "/storage/d"); r == nil || r.IsLost() {
		t.Error("/storage/d is on LTO003 and is unaffected")
	}
}

// TestOnVolumeCoversSupersededAndTombstonedVersions is the query damage
// assessment runs. Restricting it to live records would silently spare every
// historical version the volume held, which is exactly what reclamation
// destroys and what a media failure takes away.
func TestOnVolumeCoversSupersededAndTombstonedVersions(t *testing.T) {
	cat := history()

	got := cat.OnVolume("LTO001")
	var paths []string
	for _, r := range got {
		paths = append(paths, r.Path)
	}
	// a (tombstoned at gen 3), b (superseded at gen 2), c (tombstoned at gen 2).
	want := "/storage/a,/storage/b,/storage/c"
	if strings.Join(paths, ",") != want {
		t.Errorf("OnVolume(LTO001) = %v, want %s", paths, want)
	}
}

// TestOnVolumeFindsSplitFiles guards the case where Volume alone is empty and
// the volumes are named only in Parts.
func TestOnVolumeFindsSplitFiles(t *testing.T) {
	cat := catalogOf(&Record{Path: "/storage/split.iso", Size: 900, Gen: 1, Parts: []Part{
		{Volume: "LTO001", Offset: 0, Length: 400},
		{Volume: "LTO002", Offset: 400, Length: 500},
	}})

	for _, vol := range []string{"LTO001", "LTO002"} {
		if got := cat.OnVolume(vol); len(got) != 1 {
			t.Errorf("a split file depends on %s and should be found there, got %d records", vol, len(got))
		}
	}
	if got := cat.OnVolume("LTO003"); len(got) != 0 {
		t.Errorf("LTO003 holds none of it, got %d records", len(got))
	}
}

// TestOnVolumeIgnoresUnwrittenRecords stops an empty volume name from sweeping
// up everything not yet on media. OnVolume decides what a damaged volume cost,
// so a wrong answer here re-sources the whole archive or none of it.
func TestOnVolumeIgnoresUnwrittenRecords(t *testing.T) {
	cat := catalogOf(
		&Record{Path: "/storage/pending", Size: 10, Gen: 1},
		&Record{Path: "/storage/written", Size: 10, Volume: "LTO001", Gen: 1},
	)
	if got := cat.OnVolume(""); len(got) != 0 {
		t.Errorf("an empty volume name should match nothing, got %d records", len(got))
	}
}

// TestReclamationCannotBeCleared keeps the two loss reasons apart where it
// matters. A media marker is a current assessment and comes off when the object
// reads; an erased volume is not a matter of opinion.
func TestReclamationCannotBeCleared(t *testing.T) {
	damaged := &Record{Path: "/storage/a", Gen: 1}
	damaged.MarkLost(Lost{Gen: 9, Reason: LostMedia, Detail: "read failed"})
	if err := damaged.ClearLost(); err != nil {
		t.Errorf("a media marker should clear when the object reads again: %v", err)
	}
	if damaged.IsLost() {
		t.Error("the marker should be gone")
	}

	erased := &Record{Path: "/storage/b", Gen: 1}
	erased.MarkLost(Lost{Gen: 12, Reason: LostReclaimed, Detail: "LTO001 reclaimed for capacity"})
	if err := erased.ClearLost(); err == nil {
		t.Error("a reclamation must not be clearable: the bytes were erased on purpose")
	}
	if !erased.IsLost() {
		t.Error("the reclamation marker must survive the refused clear")
	}

	if err := (&Record{}).ClearLost(); err != nil {
		t.Errorf("clearing an unmarked record is not an error: %v", err)
	}
}

// TestMarkLostKeepsTheFirstMediaFindingButEscalates protects the discovery date
// a degradation trend is read from, while letting reclamation state the harder
// fact.
func TestMarkLostKeepsTheFirstMediaFindingButEscalates(t *testing.T) {
	r := &Record{Path: "/storage/a", Gen: 1}
	r.MarkLost(Lost{Gen: 9, Reason: LostMedia, Detail: "first failure"})
	r.MarkLost(Lost{Gen: 11, Reason: LostMedia, Detail: "seen again"})
	if r.Lost.Gen != 9 || r.Lost.Detail != "first failure" {
		t.Errorf("a repeat finding overwrote the original discovery: %+v", r.Lost)
	}

	r.MarkLost(Lost{Gen: 12, Reason: LostReclaimed, Detail: "volume erased"})
	if r.Lost.Reason != LostReclaimed {
		t.Errorf("reclamation should escalate over a media marker, got %q", r.Lost.Reason)
	}
}

// TestRoundTrip checks that a catalog survives the trip to disk intact,
// including the split-file records a restore depends on and the two fields the
// planner's schema did not have.
func TestRoundTrip(t *testing.T) {
	recs := []*Record{
		{Path: "/storage/b.mkv", Size: 200, ModTime: 2, Hash: "bb", HashKind: HashKindSample, Volume: "LTO002", Gen: 3},
		{Path: "/storage/a.mkv", Size: 100, ModTime: 1, Hash: "aa", HashKind: HashKindSample, Volume: "LTO001", Gen: 3,
			Object: "9f86d081884c7d65", Cipher: "aes-256-gcm"},
		{Path: "/storage/link", Size: 0, Link: "/storage/a.mkv", Gen: 3},
		{Path: "/storage/gone.mkv", Size: 50, Volume: "LTO001", Gen: 1,
			Lost: &Lost{Gen: 4, Reason: LostMedia, Detail: "LTO001 unreadable during verify 2026-09-03"}},
		{Path: "/storage/split.iso", Size: 900, Gen: 3, Parts: []Part{
			{Volume: "LTO001", Offset: 0, Length: 400},
			{Volume: "LTO002", Offset: 400, Length: 500},
		}},
	}

	for _, name := range []string{"catalog.jsonl", "catalog.jsonl.gz"} {
		path := filepath.Join(t.TempDir(), name)
		if err := Write(path, Header{Generation: 3, Note: "test"}, recs); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := Load(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Header.Generation != 3 || got.Header.Format != Format {
			t.Errorf("%s: header = %+v", name, got.Header)
		}
		if len(got.Records) != len(recs) {
			t.Fatalf("%s: wrote %d records, read %d", name, len(recs), len(got.Records))
		}
		// Records come back in path order regardless of input order.
		if got.Records[0].Path != "/storage/a.mkv" {
			t.Errorf("%s: records are not path-sorted, first is %s", name, got.Records[0].Path)
		}
		split, ok := got.Lookup("/storage/split.iso")
		if !ok || len(split.Parts) != 2 || split.Parts[1].Offset != 400 || split.Parts[1].Volume != "LTO002" {
			t.Errorf("%s: split record did not survive: %+v", name, split)
		}
		enc, ok := got.Lookup("/storage/a.mkv")
		if !ok || enc.Cipher != "aes-256-gcm" || enc.ObjectName() != "9f86d081884c7d65" {
			t.Errorf("%s: the per-object cipher and object name did not survive: %+v", name, enc)
		}
		lost, ok := got.Lookup("/storage/gone.mkv")
		if !ok || !lost.IsLost() || lost.Lost.Reason != LostMedia || lost.Lost.Gen != 4 {
			t.Errorf("%s: the loss marker did not survive: %+v", name, lost)
		}
	}
}

// TestRecordJSONStaysMinimal keeps the promise the field names are for: a
// catalog has 1.66 M lines and has to stay readable in a text editor. An
// ordinary record should carry no marker fields at all, which is also what
// makes Lost a pointer rather than a value.
func TestRecordJSONStaysMinimal(t *testing.T) {
	b, err := json.Marshal(&Record{Path: "/storage/a", Size: 10, ModTime: 1, Volume: "LTO001", Gen: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"lost", "cipher", "object", "parts", "deleted_gen"} {
		if strings.Contains(string(b), `"`+absent+`"`) {
			t.Errorf("an ordinary record should not carry %q: %s", absent, b)
		}
	}
	if !strings.Contains(string(b), `"volume":"LTO001"`) {
		t.Errorf("the volume is not named in the record: %s", b)
	}
}

func TestLoadRejectsUnknownFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.jsonl")
	if err := os.WriteFile(path, []byte(`{"_format":"something-else/9"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("expected a catalog written by an incompatible version to be refused")
	}
}

func TestLoadMissingFileIsFirstGeneration(t *testing.T) {
	cat, err := Load(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil {
		t.Fatalf("a missing catalog should mean generation 1, not an error: %v", err)
	}
	if len(cat.Records) != 0 || cat.MaxGen() != 0 {
		t.Errorf("expected an empty catalog, got %d records at gen %d", len(cat.Records), cat.MaxGen())
	}
}

// TestWriteReplacesAtomically is the reason Write goes through a temporary
// file. A failed write must leave the previous catalog readable: it is the only
// record of where the bytes are, and losing it strands every volume on the
// shelf.
func TestWriteReplacesAtomically(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions this test uses to force a failure")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.jsonl")

	if err := Write(path, Header{Generation: 1}, []*Record{
		{Path: "/storage/a", Size: 10, Volume: "LTO001", Gen: 1},
	}); err != nil {
		t.Fatal(err)
	}

	// A read-only directory stands in for a full disk: the new catalog cannot
	// be staged, so the old one must survive untouched.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	err := Write(path, Header{Generation: 2}, []*Record{
		{Path: "/storage/b", Size: 20, Volume: "LTO002", Gen: 2},
	})
	if err == nil {
		t.Fatal("writing into a read-only directory should have failed")
	}

	cat, err := Load(path)
	if err != nil {
		t.Fatalf("the previous catalog should still be readable: %v", err)
	}
	if cat.Header.Generation != 1 || len(cat.Records) != 1 || cat.Records[0].Path != "/storage/a" {
		t.Errorf("the failed write damaged the previous catalog: gen %d, %d records",
			cat.Header.Generation, len(cat.Records))
	}
}

// TestWriteLeavesNoTemporaryFileBehind keeps a run of failed writes from
// filling the catalog directory with debris.
func TestWriteLeavesNoTemporaryFileBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.jsonl")
	if err := Write(path, Header{Generation: 1}, []*Record{{Path: "/storage/a", Gen: 1}}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "catalog.jsonl" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected only the catalog, found %v", names)
	}
}

func TestVolumesListsEveryMediumARecordDependsOn(t *testing.T) {
	cases := []struct {
		name string
		rec  *Record
		want string
	}{
		{"whole file", &Record{Volume: "LTO001"}, "LTO001"},
		{"split file", &Record{Parts: []Part{{Volume: "LTO001"}, {Volume: "LTO002"}}}, "LTO001,LTO002"},
		{"repeated part", &Record{Parts: []Part{{Volume: "LTO001"}, {Volume: "LTO001"}}}, "LTO001"},
		{"not yet written", &Record{}, ""},
	}
	for _, c := range cases {
		if got := strings.Join(c.rec.Volumes(), ","); got != c.want {
			t.Errorf("%s: Volumes() = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestObjectNameFallsBackToPath covers the plain-archive layout, where an
// object is stored under its own path and no name is recorded.
func TestObjectNameFallsBackToPath(t *testing.T) {
	if got := (&Record{Path: "/storage/a"}).ObjectName(); got != "/storage/a" {
		t.Errorf("ObjectName() = %q, want the path", got)
	}
	if got := (&Record{Path: "/storage/a", Object: "abc123"}).ObjectName(); got != "abc123" {
		t.Errorf("ObjectName() = %q, want the recorded object name", got)
	}
}

// Tombstone is the producer side of DESIGN §5, and it went missing for long
// enough to be worth naming: DeletedGen, Tombstoned, GoneBy, At and Diff were
// all built and all tested, against records whose DeletedGen the tests set by
// hand. Nothing ever set it in anger, so `at-gen` listed files deleted
// generations ago and a restore of the newest generation recreated them.
func TestTombstoneMarksPathsAsGone(t *testing.T) {
	cat := catalogOf(
		&Record{Path: "/storage/x", Size: 10, Volume: "LTO001", Gen: 1},
		&Record{Path: "/storage/y", Size: 20, Volume: "LTO001", Gen: 1},
	)

	if n := cat.Tombstone([]string{"/storage/x"}, 2); n != 1 {
		t.Fatalf("Tombstone marked %d records, want 1", n)
	}
	if _, ok := cat.Lookup("/storage/x"); ok {
		t.Error("the path was deleted at generation 2 and must not look up as live")
	}
	if _, ok := cat.Lookup("/storage/y"); !ok {
		t.Error("an untouched path must stay live")
	}
	if findPath(cat.At(1), "/storage/x") == nil {
		t.Error("At(1): the path existed then and must still be restorable")
	}
	if r := findPath(cat.At(2), "/storage/x"); r != nil {
		t.Errorf("At(2): the path was gone, but got %+v", r)
	}
}

// The rule DESIGN §5 calls load-bearing: existence is a property of the newest
// version. Marking every version instead would take the path out of every past
// generation too, which is the opposite of what an archive that never reclaims
// space is for.
func TestTombstoneMarksOnlyTheNewestVersion(t *testing.T) {
	cat := catalogOf(
		&Record{Path: "/storage/x", Size: 10, Volume: "LTO001", Gen: 1},
		&Record{Path: "/storage/x", Size: 15, Volume: "LTO002", Gen: 3},
	)
	cat.Tombstone([]string{"/storage/x"}, 5)

	versions := cat.Versions("/storage/x")
	if versions[0].Tombstoned() {
		t.Error("the generation-1 version was tombstoned; its bytes are still the right answer for generations 1 and 2")
	}
	if !versions[1].Tombstoned() {
		t.Error("the newest version was not tombstoned")
	}
	if r := findPath(cat.At(2), "/storage/x"); r == nil || r.Size != 10 {
		t.Errorf("At(2) should still give the 10-byte version, got %+v", r)
	}
}

// Deletions belong to a generation, not to a cartridge, so the writer applies
// them once per volume of that generation. Re-marking has to be free, and must
// never move a tombstone forward onto a later generation than the one the path
// actually disappeared in.
func TestTombstoneIsIdempotent(t *testing.T) {
	cat := catalogOf(&Record{Path: "/storage/x", Size: 10, Volume: "LTO001", Gen: 1})

	if n := cat.Tombstone([]string{"/storage/x"}, 2); n != 1 {
		t.Fatalf("first Tombstone marked %d, want 1", n)
	}
	for _, gen := range []int{2, 3, 4} {
		if n := cat.Tombstone([]string{"/storage/x"}, gen); n != 0 {
			t.Errorf("Tombstone at generation %d marked %d records, want 0", gen, n)
		}
	}
	if got := cat.Versions("/storage/x")[0].DeletedGen; got != 2 {
		t.Errorf("DeletedGen = %d, want it pinned at 2", got)
	}
}

// A path the catalog has never heard of is not an error. The writer applies the
// plan's deletion list on every volume, and a plan can outlive the catalog
// state it was computed against.
func TestTombstoneIgnoresUnknownPaths(t *testing.T) {
	cat := catalogOf(&Record{Path: "/storage/x", Size: 10, Gen: 1})
	if n := cat.Tombstone([]string{"/storage/nope", ""}, 2); n != 0 {
		t.Errorf("Tombstone marked %d records for paths that do not exist, want 0", n)
	}
}

// Diff already treats a tombstoned record as absent, so a path that comes back
// is genuinely new rather than unchanged. Worth pinning: it is what makes a
// delete-then-restore cycle write the bytes again instead of trusting a record
// whose path no longer exists.
func TestATombstonedPathThatReturnsIsNotFoundLive(t *testing.T) {
	cat := catalogOf(&Record{Path: "/storage/x", Size: 10, Gen: 1})
	cat.Tombstone([]string{"/storage/x"}, 2)

	if _, ok := cat.Lookup("/storage/x"); ok {
		t.Fatal("a tombstoned path must not look up as live")
	}
	cat.Add(&Record{Path: "/storage/x", Size: 99, Gen: 3})
	r, ok := cat.Lookup("/storage/x")
	if !ok || r.Size != 99 {
		t.Errorf("re-adding the path should make it live again at the new version, got %+v", r)
	}
}

// TestReadHeaderCostsOneLine is why ReadHeader exists rather than Load: every
// command has to resolve which archive a workdir holds before it does anything
// else, and Load pulls in 1.16 GB of heap and six seconds at the real
// archive's 1.66 M paths to answer it.
func TestReadHeaderCostsOneLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.jsonl.gz")
	want := Header{Format: Format, Archive: "0123456789abcdef", Generation: 7}
	recs := []*Record{
		{Path: "/a", Size: 1, Gen: 7},
		{Path: "/b", Size: 2, Gen: 7},
	}
	if err := Write(path, want, recs); err != nil {
		t.Fatal(err)
	}

	got, err := ReadHeader(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Archive != want.Archive || got.Generation != want.Generation {
		t.Errorf("read back %+v, wrote %+v", got, want)
	}
}

// TestReadHeaderSaysWhenThereIsNoHeader. Load treats the header as optional and
// answers with a zero value, which a caller asking "which archive is this?"
// cannot tell apart from an archive with no ID. ReadHeader has to be explicit
// or a headerless catalog would silently resolve as anonymous.
func TestReadHeaderSaysWhenThereIsNoHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "headerless.jsonl")
	if err := os.WriteFile(path, []byte(`{"path":"/a","size":1}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := ReadHeader(path); !errors.Is(err, ErrNoHeader) {
		t.Fatalf("a headerless catalog gave %v, wanted ErrNoHeader", err)
	}
}

// TestReadHeaderOnAnEmptyFile: a catalog that exists but holds nothing is what
// an interrupted first write can leave, and it has no identity to report.
func TestReadHeaderOnAnEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := ReadHeader(path); !errors.Is(err, ErrNoHeader) {
		t.Fatalf("an empty catalog gave %v, wanted ErrNoHeader", err)
	}
}

// TestReadHeaderOnAMissingFile hands the error back rather than reporting an
// anonymous archive. Load treats a missing catalog as an empty one — that is
// the first generation — but a caller resolving identity has to know the
// difference between "no archive here" and "nothing written yet".
func TestReadHeaderOnAMissingFile(t *testing.T) {
	_, err := ReadHeader(filepath.Join(t.TempDir(), "absent.jsonl.gz"))
	if !os.IsNotExist(err) {
		t.Fatalf("a missing catalog gave %v, wanted a not-exist error", err)
	}
}

// TestReadHeaderOnCorruptGzip. The identity read happens before anything else,
// so it is the first thing to meet a damaged local catalog, and saying "gzip"
// points at the file rather than at the archive.
func TestReadHeaderOnCorruptGzip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "truncated.jsonl.gz")
	if err := os.WriteFile(path, []byte("not gzip at all"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := ReadHeader(path)
	if err == nil || !strings.Contains(err.Error(), "gzip") {
		t.Fatalf("a corrupt gzip catalog gave %v, wanted a gzip error", err)
	}
}

// TestReadHeaderOnAMalformedHeader: the line looks like a header — it carries a
// _format — but does not parse. Reporting that as "no header" would send the
// operator looking for a missing line rather than a broken one.
func TestReadHeaderOnAMalformedHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.jsonl")
	if err := os.WriteFile(path, []byte(`{"_format":"kura-catalog/1","timeline":7}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := ReadHeader(path)
	if err == nil || errors.Is(err, ErrNoHeader) {
		t.Fatalf("a malformed header gave %v, wanted a parse error", err)
	}
	if !strings.Contains(err.Error(), "catalog header") {
		t.Errorf("the error should name what failed to parse: %v", err)
	}
}
