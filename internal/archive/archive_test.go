package archive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kura/internal/catalog"
	"kura/internal/plan"
	"kura/internal/scan"
	"kura/internal/volume"
	"kura/internal/volume/filevol"
)

// fixture is one archive under test: a source tree, a shelf of volumes, and the
// catalog that accumulates as they are written.
type fixture struct {
	t     *testing.T
	src   string
	shelf string
	cat   *catalog.Catalog
	lay   plan.Layout
	cap   int64
}

func newFixture(t *testing.T, capacity int64, lay plan.Layout) *fixture {
	t.Helper()
	base := t.TempDir()
	f := &fixture{
		t:     t,
		src:   filepath.Join(base, "src"),
		shelf: filepath.Join(base, "shelf"),
		cat:   catalog.New(),
		lay:   lay,
		cap:   capacity,
	}
	f.cat.Header.Archive = "test-archive"
	if err := os.MkdirAll(f.src, 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

// write materialises files with deterministic, content-addressed bytes so a
// restored tree can be compared byte for byte.
func (f *fixture) write(spec map[string]int) {
	f.t.Helper()
	for rel, size := range spec {
		p := filepath.Join(f.src, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			f.t.Fatal(err)
		}
		buf := make([]byte, size)
		for i := range buf {
			buf[i] = byte((i*31 + len(rel)*7) % 251)
		}
		if err := os.WriteFile(p, buf, 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
}

// plan runs a real scan and pack over the source tree.
func (f *fixture) plan(opts ...func(*plan.PackOptions)) *plan.Plan {
	f.t.Helper()
	files, stats, err := scan.Walk([]scan.Root{{Scan: f.src}}, scan.NewExcluder(nil, true), func(string, error) {})
	if err != nil {
		f.t.Fatal(err)
	}
	d := scan.Diff(files, f.cat, scan.Options{Match: scan.MatchSample, Jobs: 2}, func(string, error) {})

	gen := f.cat.MaxGen() + 1
	lay := f.lay
	lay.Generation = gen
	// A small reserve, because these volumes are megabytes rather than
	// terabytes and the default floor would swallow them whole. It still has to
	// be non-zero: a volume with no room for its own header cannot be written.
	po := plan.PackOptions{
		Capacity: f.cap, MaxBlanks: -1, NamePrefix: "V",
		Layout: lay, MetadataReserve: 64 << 10,
	}
	for _, o := range opts {
		o(&po)
	}
	items := plan.Group(d.ToWrite(), 1<<40, lay)
	p := plan.Pack(items, nil, po)
	p.Generation = gen
	p.Finalize(d, stats, items)
	return p
}

// mount opens one volume off the shelf, creating it if this is its first use.
func (f *fixture) mount(id string) (volume.Volume, func()) {
	f.t.Helper()
	b := filevol.New(f.cap)
	if err := b.Load(filepath.Join(f.shelf, id)); err != nil {
		f.t.Fatal(err)
	}
	vol, err := b.Open(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	return vol, func() { b.Release(context.Background()) }
}

// writeAll executes every volume of a plan, one mount at a time, exactly as the
// operator would.
func (f *fixture) writeAll(p *plan.Plan) []*Result {
	f.t.Helper()
	var out []*Result
	for _, pv := range p.Volumes {
		vol, done := f.mount(pv.ID)
		res, err := Write(context.Background(), vol, pv, f.cat, Options{
			Archive: f.cat.Header.Archive, Generation: p.Generation,
		})
		done()
		if err != nil {
			f.t.Fatalf("writing %s: %v", pv.ID, err)
		}
		out = append(out, res)
	}
	return out
}

// restoreAll mounts every volume in turn and returns where the tree landed.
func (f *fixture) restoreAll(p *plan.Plan, gen int) (string, []*RestoreResult) {
	f.t.Helper()
	dest := filepath.Join(f.t.TempDir(), "restore")
	var out []*RestoreResult
	for _, pv := range p.Volumes {
		vol, done := f.mount(pv.ID)
		res, err := Restore(context.Background(), vol, f.cat, gen, dest, true)
		done()
		if err != nil {
			f.t.Fatalf("restoring %s: %v", pv.ID, err)
		}
		out = append(out, res)
	}
	return dest, out
}

// compareTrees is the property the whole tool exists for.
func compareTrees(t *testing.T, src, restored string) {
	t.Helper()
	want := map[string][]byte{}
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		want[rel] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// The archive stores absolute paths, so a restore rebuilds the tree under
	// the destination.
	root := filepath.Join(restored, strings.TrimPrefix(src, "/"))
	got := map[string][]byte{}
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		got[rel] = b
		return nil
	})
	if err != nil {
		t.Fatalf("walking the restored tree: %v", err)
	}

	for rel, w := range want {
		g, ok := got[rel]
		if !ok {
			t.Errorf("%s was not restored", rel)
			continue
		}
		if !bytes.Equal(w, g) {
			t.Errorf("%s restored with different bytes (%d vs %d)", rel, len(w), len(g))
		}
	}
	for rel := range got {
		if _, ok := want[rel]; !ok {
			t.Errorf("%s appeared in the restore but is not in the source", rel)
		}
	}
}

// TestRoundTrip is the claim the whole tool makes: what goes on the media comes
// back off it, byte for byte, across a mix of sizes that puts some files in
// containers and some in objects of their own.
func TestRoundTrip(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{TarBelow: 1 << 16, MaxContainer: 1 << 20})
	f.write(map[string]int{
		"Movie/film1.mkv":   1 << 20,
		"Movie/film2.mkv":   (1 << 20) + 7,
		"Docs/notes.md":     4096,
		"Docs/empty.md":     0,
		"Docs/deep/a/b/c.t": 900,
		"photos/01/x.jpg":   20000,
		"photos/02/y.jpg":   31337,
	})

	p := f.plan()
	results := f.writeAll(p)

	var objects, containers int
	for _, r := range results {
		objects += r.Objects
		containers += r.Containers
		if len(r.Problems) != 0 {
			t.Errorf("%s reported problems: %v", r.Volume, r.Problems)
		}
	}
	if containers == 0 {
		t.Error("nothing was containerised; the small-file path was not exercised")
	}
	if objects == 0 {
		t.Error("nothing was written as its own object; the large-file path was not exercised")
	}

	dest, restores := f.restoreAll(p, 1)
	for _, r := range restores {
		if !r.OK() {
			t.Errorf("%s: %d failed, %d lost", r.Volume, len(r.Failed), len(r.Lost))
		}
	}
	compareTrees(t, f.src, dest)
}

// TestVerifyIsCleanOnAFreshWrite is the baseline the damage tests are measured
// against: nothing should be reported on media nothing has touched.
func TestVerifyIsCleanOnAFreshWrite(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{TarBelow: 1 << 16})
	f.write(map[string]int{"a/big.bin": 1 << 20, "a/small.txt": 500, "b/small.txt": 700})
	p := f.plan()
	f.writeAll(p)

	for _, pv := range p.Volumes {
		vol, done := f.mount(pv.ID)
		res, err := Verify(context.Background(), vol, f.cat, true)
		done()
		if err != nil {
			t.Fatal(err)
		}
		if res.Failed() {
			t.Errorf("%s: %d findings on a fresh write: %v", pv.ID, len(res.Findings), res.Findings)
		}
		if len(res.Unclaimed) != 0 {
			t.Errorf("%s: objects nothing accounts for: %v", pv.ID, res.Unclaimed)
		}
		if res.Checked == 0 {
			t.Errorf("%s: verified nothing at all", pv.ID)
		}
	}
}

// TestVerifyCatchesAFlippedByteInsideAContainer is the case that decides whether
// containers are safe to use. One member is damaged; the digest is per member,
// so exactly one file must be blamed rather than the whole container.
func TestVerifyCatchesAFlippedByteInsideAContainer(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{TarBelow: 1 << 16})
	spec := map[string]int{}
	for i := range 20 {
		spec[fmt.Sprintf("docs/f%02d.txt", i)] = 2000 + i
	}
	f.write(spec)
	p := f.plan()
	f.writeAll(p)

	victim := corruptOneMember(t, f, p.Volumes[0].ID)

	vol, done := f.mount(p.Volumes[0].ID)
	res, err := Verify(context.Background(), vol, f.cat, true)
	done()
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Findings) != 1 {
		t.Fatalf("expected exactly one damaged file, got %d: %v", len(res.Findings), res.Findings)
	}
	if res.Findings[0].Path != victim {
		t.Errorf("blamed %s, the damaged member is %s", res.Findings[0].Path, victim)
	}
	// The blast radius is the point: 19 undamaged members must still be fine.
	if res.Checked < 19 {
		t.Errorf("only %d members were checked; the rest of the container should still read", res.Checked)
	}

	// And the damage is now in the catalog, which is what makes the next cycle
	// rewrite it without any extra machinery.
	rec, ok := f.cat.Lookup(victim)
	if !ok || !rec.IsLost() {
		t.Errorf("%s was not marked lost", victim)
	}
	if rec.Lost.Reason != catalog.LostMedia {
		t.Errorf("loss reason is %q, want media", rec.Lost.Reason)
	}
}

// corruptOneMember flips a byte inside a container and returns the path of the
// file that byte belongs to.
func corruptOneMember(t *testing.T, f *fixture, volumeID string) string {
	t.Helper()
	var container string
	for _, rec := range f.cat.OnVolume(volumeID) {
		if rec.Inside() {
			container = rec.Container
			break
		}
	}
	if container == "" {
		t.Fatal("nothing was containerised")
	}

	path := filepath.Join(f.shelf, volumeID, filepath.FromSlash(container))
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Find a member's payload rather than a header block, so the tar structure
	// survives and the damage is attributable to one file.
	var victim *catalog.Record
	for _, rec := range f.cat.OnVolume(volumeID) {
		if rec.Container == container && rec.Size > 0 {
			victim = rec
			break
		}
	}
	if victim == nil {
		t.Fatal("no non-empty member to damage")
	}
	idx := bytes.Index(b, []byte(victim.ObjectName()))
	if idx < 0 {
		t.Fatalf("member %s not found in the container", victim.ObjectName())
	}
	// The payload starts at the next block boundary after the header.
	payload := (idx/512 + 1) * 512
	if payload >= len(b) {
		t.Fatal("could not locate the member payload")
	}
	b[payload] ^= 0xFF
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return victim.Path
}

// TestVerifyCatchesADamagedObject covers the same failure for a file large
// enough to have an object of its own.
func TestVerifyCatchesADamagedObject(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{TarBelow: -1}) // no containers at all
	f.write(map[string]int{"a.bin": 4096, "b.bin": 4096})
	p := f.plan()
	f.writeAll(p)

	rec, ok := f.cat.Lookup(filepath.Join(f.src, "a.bin"))
	if !ok {
		t.Fatal("a.bin was not recorded")
	}
	target := filepath.Join(f.shelf, rec.Volume, filepath.FromSlash(rec.ObjectName()))
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	b[100] ^= 0xFF
	if err := os.WriteFile(target, b, 0o644); err != nil {
		t.Fatal(err)
	}

	vol, done := f.mount(rec.Volume)
	res, err := Verify(context.Background(), vol, f.cat, true)
	done()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 1 || res.Findings[0].Path != rec.Path {
		t.Errorf("expected a.bin to be the only finding, got %v", res.Findings)
	}
	if !rec.IsLost() {
		t.Error("the damaged record was not marked lost")
	}
}

// TestRestoreRefusesToLookComplete is DESIGN §8.1d. Borrowed from borg's
// zero-chunk behaviour, which is loud on purpose: a restore that cannot produce
// an object must say so per object, never write a directory that looks finished.
func TestRestoreRefusesToLookComplete(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{TarBelow: -1})
	f.write(map[string]int{"keep.bin": 2048, "gone.bin": 2048})
	p := f.plan()
	f.writeAll(p)

	rec, ok := f.cat.Lookup(filepath.Join(f.src, "gone.bin"))
	if !ok {
		t.Fatal("gone.bin was not recorded")
	}
	// Media failed and verify recorded it, which is the ordinary way a restore
	// meets a hole.
	rec.MarkLost(catalog.Lost{Gen: 1, Reason: catalog.LostMedia, Detail: "unreadable"})

	dest, results := f.restoreAll(p, 1)
	var lost int
	for _, r := range results {
		lost += len(r.Lost)
		if r.OK() && len(r.Lost) > 0 {
			t.Error("a restore with a known hole reported itself as OK")
		}
	}
	if lost != 1 {
		t.Errorf("expected the lost file to be named once, got %d", lost)
	}
	// The rest of the tree is still produced: an incomplete restore is more
	// useful than none, as long as it says what is missing.
	if _, err := os.Stat(filepath.Join(dest, strings.TrimPrefix(f.src, "/"), "keep.bin")); err != nil {
		t.Errorf("the undamaged file should still be restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, strings.TrimPrefix(f.src, "/"), "gone.bin")); !os.IsNotExist(err) {
		t.Error("a file recorded as lost must not appear in the restore")
	}
}

// TestRestoreDetectsCorruptionOnTheWayOut keeps damage from reaching the
// restored tree even when verify has not been run.
func TestRestoreDetectsCorruptionOnTheWayOut(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{TarBelow: -1})
	f.write(map[string]int{"a.bin": 4096})
	p := f.plan()
	f.writeAll(p)

	rec, _ := f.cat.Lookup(filepath.Join(f.src, "a.bin"))
	target := filepath.Join(f.shelf, rec.Volume, filepath.FromSlash(rec.ObjectName()))
	b, _ := os.ReadFile(target)
	b[10] ^= 0xFF
	os.WriteFile(target, b, 0o644)

	dest, results := f.restoreAll(p, 1)
	if len(results[0].Failed) != 1 {
		t.Fatalf("expected the corrupted file to fail, got %d failures", len(results[0].Failed))
	}
	// Nothing under the real name: a half-trusted file is worse than none.
	if _, err := os.Stat(filepath.Join(dest, strings.TrimPrefix(f.src, "/"), "a.bin")); !os.IsNotExist(err) {
		t.Error("a file that failed its digest must not be left behind under its real name")
	}
}

// TestResumeAfterAnInterruptedWrite is the crash path. Put is atomic, so a
// killed writer leaves whole objects and no catalog; the next run has to
// recognise them rather than fail on the first name it already used.
func TestResumeAfterAnInterruptedWrite(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{TarBelow: 1 << 16})
	f.write(map[string]int{
		"big.bin":   1 << 20,
		"docs/a.md": 900,
		"docs/b.md": 950,
	})
	p := f.plan()
	pv := p.Volumes[0]

	// Stand in for a run killed partway: place the objects, keep no catalog.
	scratch := catalog.New()
	scratch.Header.Archive = f.cat.Header.Archive
	vol, done := f.mount(pv.ID)
	if _, err := Write(context.Background(), vol, pv, scratch, Options{
		Archive: f.cat.Header.Archive, Generation: 1,
	}); err != nil {
		t.Fatal(err)
	}
	done()

	// The real run, against a catalog that remembers nothing.
	vol, done = f.mount(pv.ID)
	res, err := Write(context.Background(), vol, pv, f.cat, Options{
		Archive: f.cat.Header.Archive, Generation: 1,
	})
	done()
	if err != nil {
		t.Fatalf("a resumed write must not fail on names it already placed: %v", err)
	}
	if res.Resumed == 0 {
		t.Error("nothing was recognised as already written")
	}
	if res.Objects != 0 || res.Containers != 0 {
		t.Errorf("a full resume should write nothing new, wrote %d objects and %d containers",
			res.Objects, res.Containers)
	}
	// Everything is still described, with digests read back off the medium.
	if len(res.Records) != 3 {
		t.Fatalf("expected all 3 files described after the resume, got %d", len(res.Records))
	}
	for _, rec := range res.Records {
		if rec.FullHash == "" {
			t.Errorf("%s came back from a resume with no digest", rec.Path)
		}
	}

	dest, _ := f.restoreAll(p, 1)
	compareTrees(t, f.src, dest)
}

// TestForeignVolumeIsRefused is the row of the reconciliation table that stops
// a cartridge from another archive being written over (DESIGN §11).
func TestForeignVolumeIsRefused(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{})
	f.write(map[string]int{"a.bin": 1024})
	p := f.plan()

	vol, done := f.mount(p.Volumes[0].ID)
	if _, err := Reconcile(context.Background(), vol, "somebody-elses-archive", 1, false); err != nil {
		t.Fatal(err)
	}
	done()

	vol, done = f.mount(p.Volumes[0].ID)
	_, err := Write(context.Background(), vol, p.Volumes[0], f.cat, Options{
		Archive: "test-archive", Generation: 1,
	})
	done()
	if !errors.Is(err, ErrForeign) {
		t.Errorf("expected a foreign volume to be refused, got %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), "somebody-elses-archive") {
		t.Errorf("the error should name the archive the volume belongs to: %v", err)
	}
}

// TestBlankVolumeIsStamped covers the other end: a volume with no headers is
// initialised, and reading them back identifies both the archive and the
// cartridge.
func TestBlankVolumeIsStamped(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{})
	f.write(map[string]int{"a.bin": 1024})
	p := f.plan()
	f.writeAll(p)

	vol, done := f.mount(p.Volumes[0].ID)
	defer done()

	h, err := ReadHeader(context.Background(), vol)
	if err != nil {
		t.Fatal(err)
	}
	if h.Magic != HeaderMagic || h.Archive != "test-archive" || h.Volume != p.Volumes[0].ID {
		t.Errorf("volume header does not identify the volume: %+v", h)
	}
	// The volume header carries identity only. A generation number here would
	// have to be rewritten every cycle, which on the target medium means
	// abandoning the old copy; generations/ carries that instead.
	newest, err := NewestGeneration(context.Background(), vol)
	if err != nil {
		t.Fatal(err)
	}
	if newest != 1 {
		t.Errorf("volume reports generation %d from its object names, want 1", newest)
	}

	ah, err := ReadArchiveHeader(context.Background(), vol)
	if err != nil {
		t.Fatal(err)
	}
	if ah.Magic != ArchiveMagic || ah.Archive != "test-archive" {
		t.Errorf("archive header does not identify the archive: %+v", ah)
	}
	if ah.Encrypted {
		t.Error("nothing here encrypts yet, but the archive header claims it does")
	}
}

// TestVolumeCarriesItsOwnCatalog is what makes an archive readable with no
// external database, and the reason more than one copy is written.
func TestVolumeCarriesItsOwnCatalog(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{TarBelow: 1 << 16})
	f.write(map[string]int{"a.bin": 1024, "b.txt": 40})
	p := f.plan()
	f.writeAll(p)

	vol, done := f.mount(p.Volumes[0].ID)
	defer done()
	objs, err := vol.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var copies []string
	for _, o := range objs {
		if isGenerationObject(o.Name) {
			copies = append(copies, o.Name)
		}
	}
	if len(copies) != DefaultCatalogCopies {
		t.Errorf("expected %d catalog copies, found %v", DefaultCatalogCopies, copies)
	}

	// Every copy has to be a usable catalog on its own; one that only parses
	// as far as the header would be worse than useless.
	for _, name := range copies {
		rc, err := vol.Get(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		tmp := filepath.Join(t.TempDir(), "read.jsonl.gz")
		b, _ := os.ReadFile("/dev/null")
		_ = b
		out, err := os.Create(tmp)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := out.ReadFrom(rc); err != nil {
			t.Fatal(err)
		}
		out.Close()
		rc.Close()

		got, err := catalog.Load(tmp)
		if err != nil {
			t.Fatalf("%s did not read back as a catalog: %v", name, err)
		}
		if len(got.Records) != len(f.cat.Records) {
			t.Errorf("%s holds %d records, the archive has %d", name, len(got.Records), len(f.cat.Records))
		}
		if got.Header.Archive != "test-archive" {
			t.Errorf("%s does not identify the archive", name)
		}
	}
}

// TestVolumeFullStopsRatherThanScattering keeps a capacity miscalculation from
// silently spilling a volume's share across the shelf.
func TestVolumeFullStopsRatherThanScattering(t *testing.T) {
	f := newFixture(t, 1<<20, plan.Layout{TarBelow: -1})
	f.write(map[string]int{"a.bin": 400 << 10, "b.bin": 400 << 10})
	p := f.plan()

	// Mount a volume far smaller than the plan assumed, as a cartridge with
	// less usable space than expected would be.
	b := filevol.New(300 << 10)
	if err := b.Load(filepath.Join(f.shelf, p.Volumes[0].ID)); err != nil {
		t.Fatal(err)
	}
	vol, err := b.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Release(context.Background())

	_, err = Write(context.Background(), vol, p.Volumes[0], f.cat, Options{
		Archive: "test-archive", Generation: 1,
	})
	if !errors.Is(err, volume.ErrVolumeFull) {
		t.Errorf("expected the write to stop with a full volume, got %v", err)
	}
}

// TestChangedSourceIsReportedNotRecorded is the race between planning and
// writing. A file that changed cannot be stored at the size the plan promised,
// and recording it anyway would put a digest of the wrong bytes in the catalog.
func TestChangedSourceIsReportedNotRecorded(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{TarBelow: -1})
	f.write(map[string]int{"steady.bin": 2048, "moving.bin": 2048})
	p := f.plan()

	// Grow it after the plan was made.
	f.write(map[string]int{"moving.bin": 9000})

	vol, done := f.mount(p.Volumes[0].ID)
	res, err := Write(context.Background(), vol, p.Volumes[0], f.cat, Options{
		Archive: "test-archive", Generation: 1,
	})
	done()
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Problems) != 1 || filepath.Base(res.Problems[0].Path) != "moving.bin" {
		t.Fatalf("expected moving.bin to be reported, got %v", res.Problems)
	}
	for _, rec := range res.Records {
		if filepath.Base(rec.Path) == "moving.bin" {
			t.Error("a file that could not be stored must not be recorded as stored")
		}
	}
	// The next cycle must see it as still needing a write.
	if _, ok := f.cat.Lookup(filepath.Join(f.src, "moving.bin")); ok {
		t.Error("the catalog claims a file that was never written")
	}
}

// TestSplitFileSpansVolumes covers a file too large for one volume: each
// fragment is an object on its own volume, verified on its own, and the restore
// refuses to name the file until every piece is in.
func TestSplitFileSpansVolumes(t *testing.T) {
	f := newFixture(t, 1<<20, plan.Layout{TarBelow: -1})
	f.write(map[string]int{"huge.bin": (2 << 20) + 12345})

	p := f.plan(func(o *plan.PackOptions) {
		o.AllowSplit = true
		o.SplitMin = 1
	})
	if len(p.Volumes) < 3 {
		t.Fatalf("expected the file to span at least 3 volumes, got %d", len(p.Volumes))
	}
	f.writeAll(p)

	rec, ok := f.cat.Lookup(filepath.Join(f.src, "huge.bin"))
	if !ok {
		t.Fatal("huge.bin was not recorded")
	}
	if len(rec.Parts) != len(p.Volumes) {
		t.Fatalf("expected %d parts, got %d", len(p.Volumes), len(rec.Parts))
	}
	var covered int64
	for _, part := range rec.Parts {
		if part.Hash == "" {
			t.Errorf("fragment on %s has no digest, so that volume cannot be verified alone", part.Volume)
		}
		covered += part.Length
	}
	if covered != rec.Size {
		t.Errorf("fragments cover %d bytes, the file is %d", covered, rec.Size)
	}

	// Each volume verifies on its own, which is the point of a per-part digest.
	for _, pv := range p.Volumes {
		vol, done := f.mount(pv.ID)
		res, err := Verify(context.Background(), vol, f.cat, false)
		done()
		if err != nil {
			t.Fatal(err)
		}
		if res.Failed() {
			t.Errorf("%s: %v", pv.ID, res.Findings)
		}
	}

	dest, results := f.restoreAll(p, 1)
	// Every mount but the last leaves the file partial and says so.
	for _, r := range results[:len(results)-1] {
		if len(r.Incomplete) != 1 {
			t.Errorf("%s should have reported the file as incomplete, got %v", r.Volume, r.Incomplete)
		}
	}
	compareTrees(t, f.src, dest)
}

// TestObjectNamesStayInsideTheVolume checks the layer above the backend does not
// hand it a name that would escape. Names reach a backend from a catalog that
// may have been read off media someone else wrote.
func TestObjectNamesStayInsideTheVolume(t *testing.T) {
	cases := []string{
		"/storage/a", "storage/../../etc/passwd", "/../../etc/passwd", "//storage//a",
	}
	for _, in := range cases {
		for _, got := range []string{dataObjectName(in), memberName(in), fragmentObjectName(in, 1)} {
			if strings.HasPrefix(got, "/") {
				t.Errorf("%q produced the absolute name %q", in, got)
			}
			if got == ".." || strings.HasPrefix(got, "../") {
				t.Errorf("%q produced %q, which climbs out of the volume", in, got)
			}
		}
	}
}

// TestDryRunTouchesNothing keeps --dry-run honest: it reads and digests
// everything, so its answer is worth something, and writes nothing at all.
func TestDryRunTouchesNothing(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{TarBelow: 1 << 16})
	f.write(map[string]int{"a.bin": 1 << 19, "b.txt": 400})
	p := f.plan()

	vol, done := f.mount(p.Volumes[0].ID)
	res, err := Write(context.Background(), vol, p.Volumes[0], f.cat, Options{
		Archive: "test-archive", Generation: 1, DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	objs, err := vol.List(context.Background())
	done()
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 0 {
		t.Errorf("a dry run left %d objects behind: %v", len(objs), objs)
	}
	if len(res.Records) != 2 {
		t.Errorf("a dry run should still describe what it would write, got %d records", len(res.Records))
	}
	for _, rec := range res.Records {
		if rec.FullHash == "" {
			t.Errorf("%s has no digest; the dry run did not actually read it", rec.Path)
		}
	}
}

// The archive header is stamped before the volume header, and a run that dies
// between the two must be finishable.
//
// This is the state that used to be unrecoverable, and it was reachable by a
// kill, a power cut, or a medium that turned out too small: the medium carried
// an archive ID that the local catalog had never been told about. Reconcile now
// treats a stamped archive header with no volume header as a half-finished
// stamp rather than as a foreign volume, because the archive check has already
// established that the identity matches.
func TestReconcileFinishesAnInterruptedStamp(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{})
	ctx := context.Background()

	vol, done := f.mount("LTO001")
	if err := putJSON(ctx, vol, ArchiveObject, &ArchiveHeader{
		Magic: ArchiveMagic, Format: HeaderFormat, Archive: "test-archive",
	}); err != nil {
		t.Fatal(err)
	}
	done()

	vol, done = f.mount("LTO001")
	defer done()
	h, err := Reconcile(ctx, vol, "test-archive", 1, false)
	if err != nil {
		t.Fatalf("a half-stamped volume of our own archive was refused: %v", err)
	}
	if h.Volume != "LTO001" || h.Archive != "test-archive" {
		t.Errorf("the volume header does not identify the volume: %+v", h)
	}
}

// ArchiveIDOn is what lets a catalog that lost its archive ID recover it from
// any cartridge instead of minting a second identity for an archive that
// already has one.
func TestArchiveIDCanBeReadBackFromTheMedium(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{})
	f.write(map[string]int{"a.bin": 1024})
	p := f.plan()
	f.writeAll(p)

	vol, done := f.mount(p.Volumes[0].ID)
	defer done()
	got, err := ArchiveIDOn(context.Background(), vol)
	if err != nil {
		t.Fatal(err)
	}
	if got != "test-archive" {
		t.Errorf("ArchiveIDOn = %q, want test-archive", got)
	}
}

// A blank cartridge has no identity to offer, and saying so is what tells the
// caller to mint one rather than to adopt an empty string.
func TestArchiveIDOnABlankVolumeIsEmpty(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{})
	vol, done := f.mount("LTO001")
	defer done()

	got, err := ArchiveIDOn(context.Background(), vol)
	if err != nil {
		t.Fatalf("a blank volume should not be an error: %v", err)
	}
	if got != "" {
		t.Errorf("ArchiveIDOn = %q on a blank volume, want empty", got)
	}
}

// The two plaintext objects are cross-checked. A volume carrying pieces of two
// archives is corrupt rather than foreign, and conflating the two would send
// the operator looking for the wrong problem.
func TestHeadersThatDisagreeAreRefused(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{})
	ctx := context.Background()

	vol, done := f.mount("LTO001")
	if err := putJSON(ctx, vol, ArchiveObject, &ArchiveHeader{
		Magic: ArchiveMagic, Format: HeaderFormat, Archive: "test-archive",
	}); err != nil {
		t.Fatal(err)
	}
	if err := putJSON(ctx, vol, HeaderObject, &VolumeHeader{
		Magic: HeaderMagic, Format: HeaderFormat, Archive: "a-different-archive", Volume: "LTO001",
	}); err != nil {
		t.Fatal(err)
	}
	done()

	vol, done = f.mount("LTO001")
	defer done()
	_, err := Reconcile(ctx, vol, "test-archive", 1, false)
	if err == nil {
		t.Fatal("a volume whose two headers name different archives was accepted")
	}
	if !strings.Contains(err.Error(), "disagrees") {
		t.Errorf("the error should say the volume's own metadata disagrees: %v", err)
	}
	if errors.Is(err, ErrForeign) {
		t.Error("a self-contradictory volume is corrupt, not foreign")
	}
}

// Reconcile without an archive ID would stamp the empty string onto a blank
// cartridge and make it unmatchable by anything afterwards.
func TestReconcileRefusesAnEmptyArchiveID(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{})
	vol, done := f.mount("LTO001")
	defer done()

	if _, err := Reconcile(context.Background(), vol, "", 1, false); err == nil {
		t.Fatal("Reconcile accepted an empty archive ID")
	}
}

// The archive header is identical on every volume, which is what will let a
// KDF salt live in it once encryption ships (DESIGN §3.1, §9.1): an object
// migrated between volumes has to stay decryptable from its own record.
func TestTheArchiveHeaderIsIdenticalOnEveryVolume(t *testing.T) {
	f := newFixture(t, 1<<20, plan.Layout{TarBelow: -1})
	f.write(map[string]int{"huge.bin": (2 << 20) + 12345})
	p := f.plan(func(o *plan.PackOptions) {
		o.AllowSplit = true
		o.SplitMin = 1
	})
	if len(p.Volumes) < 2 {
		t.Fatalf("this needs a multi-volume plan, got %d", len(p.Volumes))
	}
	f.writeAll(p)

	var first []byte
	for _, pv := range p.Volumes {
		vol, done := f.mount(pv.ID)
		rc, err := vol.Get(context.Background(), ArchiveObject)
		if err != nil {
			t.Fatalf("%s carries no archive header: %v", pv.ID, err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		done()
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = b
		} else if !bytes.Equal(first, b) {
			t.Errorf("%s carries a different archive header:\n%s\nvs\n%s", pv.ID, first, b)
		}
	}
}

// readCatalogObject pulls one on-volume catalog copy back into a Catalog.
func readCatalogObject(t *testing.T, vol volume.Volume, name string) *catalog.Catalog {
	t.Helper()
	rc, err := vol.Get(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	tmp := filepath.Join(t.TempDir(), "copy.jsonl.gz")
	out, err := os.Create(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := out.ReadFrom(rc); err != nil {
		t.Fatal(err)
	}
	out.Close()

	cat, err := catalog.Load(tmp)
	if err != nil {
		t.Fatalf("%s is not a readable catalog: %v", name, err)
	}
	return cat
}

// A second generation written to the same cartridge must leave an index that
// describes both, and this is the test that did not exist.
//
// The catalog copies were named by copy number alone, so a later generation
// found kura.catalog.000/001 already present and skipped them — leaving the
// volume carrying an index for the first cycle only. Silent, because nothing
// reads the on-volume catalog back until the local one is gone, which is the
// one moment it has to be right. The name now carries the generation, the way a
// container's already did (DESIGN §4.1).
func TestASecondGenerationOnTheSameVolumeGetsItsOwnCatalog(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{TarBelow: -1})
	f.write(map[string]int{"first.bin": 2048})
	g1 := f.plan()
	f.writeAll(g1)

	f.write(map[string]int{"second.bin": 4096})
	g2 := f.plan()
	if g2.Generation != 2 {
		t.Fatalf("second plan is generation %d, want 2", g2.Generation)
	}
	if g2.Volumes[0].ID != g1.Volumes[0].ID {
		t.Fatalf("this test needs both generations on one cartridge, got %s then %s",
			g1.Volumes[0].ID, g2.Volumes[0].ID)
	}
	f.writeAll(g2)

	vol, done := f.mount(g1.Volumes[0].ID)
	defer done()

	objs, err := vol.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byGen := map[int][]string{}
	for _, o := range objs {
		if g := generationOf(o.Name); g > 0 {
			byGen[g] = append(byGen[g], o.Name)
		}
	}
	for _, gen := range []int{1, 2} {
		if len(byGen[gen]) != DefaultCatalogCopies {
			t.Errorf("generation %d has %d catalog copies on the volume, want %d: %v",
				gen, len(byGen[gen]), DefaultCatalogCopies, byGen[gen])
		}
	}

	// The generation-2 copy is the one a recovery would read, and it has to
	// know about both cycles.
	newest := readCatalogObject(t, vol, catalogObjectName(2, 0))
	for _, want := range []string{"first.bin", "second.bin"} {
		if _, ok := newest.Lookup(filepath.Join(f.src, want)); !ok {
			t.Errorf("the generation-2 catalog on the volume does not know about %s", want)
		}
	}
	if newest.Header.Generation != 2 {
		t.Errorf("the on-volume catalog says generation %d, want 2", newest.Header.Generation)
	}

	// And the generation-1 copy is left exactly as it was: write-once, so the
	// earlier index is still there to fall back on.
	older := readCatalogObject(t, vol, catalogObjectName(1, 0))
	if _, ok := older.Lookup(filepath.Join(f.src, "second.bin")); ok {
		t.Error("the generation-1 catalog was rewritten; it should still describe generation 1 only")
	}
}

// NewestGeneration answers "how far has this cartridge been written?" from
// object names, which is what replaced the generation field the volume header
// used to carry.
func TestNewestGenerationComesFromTheListing(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{TarBelow: -1})

	vol, done := f.mount("V000")
	got, err := NewestGeneration(context.Background(), vol)
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("a blank volume reports generation %d, want 0", got)
	}
	done()

	// Different lengths of name and of content: the fixture derives bytes from
	// len(name), so two same-length names produce identical files and the
	// second is classified as a re-added duplicate rather than as new.
	f.write(map[string]int{"alpha.bin": 2048})
	f.writeAll(f.plan())
	f.write(map[string]int{"beta-two.bin": 4096})
	f.writeAll(f.plan())

	vol, done = f.mount("V000")
	defer done()
	got, err = NewestGeneration(context.Background(), vol)
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Errorf("NewestGeneration = %d after two cycles, want 2", got)
	}
}

func TestGenerationOfParsesOnlyItsOwnNamespace(t *testing.T) {
	for name, want := range map[string]int{
		"generations/g0001/catalog.000.jsonl.gz": 1,
		"generations/g0042/catalog.001.jsonl.gz": 42,
		"generations/g0000/catalog.000.jsonl.gz": 0,
		"generations/nope/catalog.000.jsonl.gz":  0,
		"generations/gxx/catalog.000.jsonl.gz":   0,
		"generations/g0001":                      0,
		"containers/g0001/x.part001.tar":         0,
		"data/storage/film.mkv":                  0,
		"kura.volume.json":                       0,
	} {
		if got := generationOf(name); got != want {
			t.Errorf("generationOf(%q) = %d, want %d", name, got, want)
		}
	}
}
