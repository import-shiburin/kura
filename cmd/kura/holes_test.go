package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"kura/internal/archive"
	"kura/internal/catalog"
	"kura/internal/plan"
	"kura/internal/volume/filevol"
	"kura/internal/workdir"
)

// The tests in this file drive the real commands in-process — runPlan, then
// runWrite once per volume with a fresh cartridge each time — with the workdir
// under KURA_HOME and nothing named, exactly as an operator runs a cycle. They
// exist because the holes they document are only visible across the whole
// cycle: each layer below is correct on its own, and the file falls out of the
// archive in the seam between them.
//
// runPlan and runWrite exit the process on a refusal, so a scenario that
// expects one belongs in the VM suite. These cover the success path, where the
// question is what the catalog says afterwards.
type cycle struct {
	t     *testing.T
	root  string
	shelf string
	slots int
}

func newCycle(t *testing.T) *cycle {
	t.Helper()
	base := t.TempDir()
	t.Setenv(workdir.RootEnv, filepath.Join(base, "home"))
	c := &cycle{t: t, root: filepath.Join(base, "src"), shelf: filepath.Join(base, "shelf")}
	for _, d := range []string{c.root, c.shelf} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func (c *cycle) workdir() workdir.Dir {
	c.t.Helper()
	d, err := workdir.Resolve("", "")
	if err != nil {
		c.t.Fatal(err)
	}
	return d
}

// plan runs `kura plan` with the flags an operator would leave at their
// defaults, which is the point: nothing here passes -start.
func (c *cycle) plan() *plan.Plan {
	c.t.Helper()
	runPlan([]string{"-root", c.root, "-capacity", "50MB", "-headroom", "0", "-quiet"})
	p, err := loadPlan(c.workdir().Plan())
	if err != nil {
		c.t.Fatal(err)
	}
	return p
}

// writeAll runs `kura write` once per volume of the plan, each against a blank
// cartridge in a fresh slot. A plan with no volumes is still written once,
// because a generation that only removed or moved files has to be recorded.
func (c *cycle) writeAll(p *plan.Plan) {
	c.t.Helper()
	if len(p.Volumes) == 0 {
		runWrite([]string{"-mount", c.shelf, "-allow-any-dir", "-quiet"})
		return
	}
	for range p.Volumes {
		c.slots++
		slot := filepath.Join(c.shelf, "slot"+strconv.Itoa(c.slots))
		if err := os.MkdirAll(slot, 0o755); err != nil {
			c.t.Fatal(err)
		}
		runWrite([]string{"-mount", slot, "-allow-any-dir", "-quiet"})
	}
}

func (c *cycle) catalog() *catalog.Catalog {
	c.t.Helper()
	cat, err := catalog.Load(c.workdir().Catalog())
	if err != nil {
		c.t.Fatal(err)
	}
	return cat
}

// TestARenamedFileStaysInTheArchive documents a hole in the seam between the
// scan and the writer.
//
// Diff recognises a moved file and puts the pair in Delta.Reused, so nothing
// is written for it — correct, the bytes are on media. Finalize then keeps
// only the count, the plan file has no field for the pair, and write has
// nothing to record. The vacated path is tombstoned, the new path is never
// written and never recorded, and the report says the bytes were "avoided".
// The file is in no generation from then on, and the next plan matches it and
// drops it again; only -match off, which rewrites it, gets it back.
func TestARenamedFileStaysInTheArchive(t *testing.T) {
	c := newCycle(t)
	writeTree(t, c.root, map[string]int{"Movie/film.mkv": 3 << 20, "Docs/notes.md": 900})
	c.writeAll(c.plan())

	old := filepath.Join(c.root, "Movie/film.mkv")
	renamed := filepath.Join(c.root, "Film/film.mkv")
	moveFile(t, old, renamed)

	p := c.plan()
	if p.Summary.ReusedFiles != 1 || p.Summary.NewFiles != 0 {
		t.Fatalf("this test needs the scan to match the moved file against its existing copy, got reused=%d new=%d",
			p.Summary.ReusedFiles, p.Summary.NewFiles)
	}
	c.writeAll(p)

	cat := c.catalog()
	rec, ok := cat.Lookup(renamed)
	if !ok {
		t.Fatalf("after a cycle that reported the move as \"already written\", %s is in no generation at all", renamed)
	}
	if rec.Volume == "" || rec.Gen != p.Generation {
		t.Errorf("the moved file's record should be a generation-%d pointer at the bytes already on media, got volume %q gen %d",
			p.Generation, rec.Volume, rec.Gen)
	}
	if _, ok := cat.Lookup(old); ok {
		t.Errorf("the vacated path %s is still live", old)
	}

	// And it stays recorded: the next plan must not match it all over again.
	next := c.plan()
	if next.Summary.ReusedFiles != 0 || next.Summary.NewFiles != 0 {
		t.Errorf("the cycle after the move still sees the file as reused=%d new=%d; it was never recorded",
			next.Summary.ReusedFiles, next.Summary.NewFiles)
	}
}

// TestARefreshedRecordSettlesOnStatDataNextCycle is the cheaper half of the
// same seam. A file whose ctime moved but whose content was verified identical
// lands in Delta.Refreshed, which exists so the catalog can take the new ctime
// and the next cycle can settle the file from stat data alone. Nothing carries
// it, so after a chmod -R every file is re-read in full on every cycle forever.
func TestARefreshedRecordSettlesOnStatDataNextCycle(t *testing.T) {
	c := newCycle(t)
	writeTree(t, c.root, map[string]int{"a.bin": 2 << 20, "b.bin": 3 << 20})
	c.writeAll(c.plan())

	// A permission change: ctime moves, size, mtime and content do not.
	if err := os.Chmod(filepath.Join(c.root, "a.bin"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := c.plan()
	if p.Summary.HashedFiles != 1 || p.Summary.ModifiedFiles != 0 {
		t.Fatalf("this test needs the file read once and found identical, got hashed=%d modified=%d",
			p.Summary.HashedFiles, p.Summary.ModifiedFiles)
	}
	c.writeAll(p)

	next := c.plan()
	if next.Summary.HashedFiles != 0 {
		t.Errorf("a file verified identical last cycle was read again (%d hashed); the refreshed ctime never reached the catalog",
			next.Summary.HashedFiles)
	}
}

// TestPlanNeverReusesAVolumeIDTheArchiveHasWritten: plan numbers volumes from
// -start, which defaults to 1 on every run and is never checked against the
// catalog, so the second generation names LTO001 again. Two cartridges then
// answer to one ID — verify of either marks the other's records lost — or the
// gen-1 cartridge is accepted for gen 2 and a modified file is "resumed" from
// its old bytes. The e2e suite passes -start by hand to avoid it.
func TestPlanNeverReusesAVolumeIDTheArchiveHasWritten(t *testing.T) {
	c := newCycle(t)
	writeTree(t, c.root, map[string]int{"a/one.bin": 3 << 20})
	c.writeAll(c.plan())

	writeTree(t, c.root, map[string]int{"b/two.bin": 4 << 20}) // something for generation 2 to write
	p := c.plan()
	if len(p.Volumes) == 0 {
		t.Fatal("generation 2 has nothing to write; the test needs a volume")
	}

	written := map[string]bool{}
	for _, v := range c.catalog().Header.Volumes {
		written[v.ID] = true
	}
	for _, pv := range p.Volumes {
		if written[pv.ID] {
			t.Errorf("generation %d plans to write %s, which the catalog already lists as written: two cartridges would answer to one ID",
				p.Generation, pv.ID)
		}
	}
}

// TestFirstWriteRefusesAForeignCartridge: a new archive's first write starts
// from a catalog with no ID. ensureArchiveID adopts whatever kura.archive.json
// the loaded cartridge carries and persists it before Reconcile runs, so the
// foreign check that follows compares the medium against an ID just copied
// from it and passes. With the default prefix both archives own an LTO001, so
// the volume check passes too: A's files land on B's cartridge under B's
// identity, A's catalog is hijacked, and A's own plan is refused afterwards.
//
// The plan is the evidence that was ignored: it names the archive its workdir
// was created for.
func TestFirstWriteRefusesAForeignCartridge(t *testing.T) {
	ctx := context.Background()
	drive := filepath.Join(t.TempDir(), "drive")
	b := filevol.New(0)
	if err := b.Load(drive); err != nil {
		t.Fatal(err)
	}
	vol, err := b.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Release(ctx)

	// Somebody else's cartridge, in the drive.
	if _, err := archive.Reconcile(ctx, vol, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "LTO001", 1, false); err != nil {
		t.Fatal(err)
	}

	// Our archive, about to write its first volume: the catalog has no ID
	// yet, the plan carries the one the workdir was named by.
	cat := catalog.New()
	p := &plan.Plan{Archive: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ID: "plan-a"}
	catalogPath := filepath.Join(t.TempDir(), "catalog.jsonl.gz")

	err = ensureArchiveID(ctx, cat, vol, p, catalogPath, false)
	if err == nil {
		t.Fatalf("a cartridge belonging to archive bbbb was adopted as the identity of archive aaaa")
	}
	if cat.Header.Archive == "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Error("the catalog now names the other archive")
	}
	if _, err := os.Stat(catalogPath); err == nil {
		t.Error("the hijacked identity was persisted before any check ran")
	}
}
