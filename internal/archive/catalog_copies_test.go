package archive

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"kura/internal/catalog"
	"kura/internal/plan"
	"kura/internal/volume"
)

// The catalog copies on a volume are what make it readable with no workdir at
// all (DESIGN §8.1a), and the one structure nothing reads back until the day
// the workdir is gone. These tests are about the ways they can be wrong while
// every report says Clean.

// readableCopies returns the generation objects that load as a catalog for
// this archive, by name.
func readableCopies(t *testing.T, vol volume.Volume, archiveID string) []string {
	t.Helper()
	objs, err := vol.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, o := range objs {
		if !isGenerationObject(o.Name) {
			continue
		}
		rc, err := vol.Get(context.Background(), o.Name)
		if err != nil {
			continue
		}
		tmp := filepath.Join(t.TempDir(), "copy.jsonl.gz")
		f, err := os.Create(tmp)
		if err != nil {
			t.Fatal(err)
		}
		_, copyErr := f.ReadFrom(rc)
		f.Close()
		rc.Close()
		if copyErr != nil {
			continue
		}
		c, err := catalog.Load(tmp)
		if err != nil || c.Header.Archive != archiveID {
			continue
		}
		out = append(out, o.Name)
	}
	sort.Strings(out)
	return out
}

// TestResumeLeavesEnoughReadableCatalogCopies: writeCatalog skips a copy whose
// name already exists, with no size or digest check. A power cut before the
// end-of-volume Sync can leave the copies zero-filled; the resumed run skips
// them, syncs, and saves locally, and the volume's own index is garbage from
// then on.
func TestResumeLeavesEnoughReadableCatalogCopies(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{TarBelow: -1})
	f.write(map[string]int{"a.bin": 4096, "b.bin": 8192})
	p := f.plan()
	pv := p.Volumes[0]
	placeWithoutCatalog(t, f, pv, p.Generation)

	zeroFill(t, filepath.Join(f.shelf, pv.ID, filepath.FromSlash(catalogObjectName(p.Generation, 0))))

	vol, done := f.mount(pv.ID)
	if _, err := Write(context.Background(), vol, pv, f.cat, Options{
		Archive: f.cat.Header.Archive, Generation: p.Generation,
	}); err != nil {
		t.Fatal(err)
	}
	done()

	vol, done = f.mount(pv.ID)
	defer done()
	copies := readableCopies(t, vol, f.cat.Header.Archive)
	if len(copies) < DefaultCatalogCopies {
		t.Errorf("after a resume over a copy that did not survive, the volume holds %d readable catalog copies (%v), want at least %d: the damaged one was trusted by name",
			len(copies), copies, DefaultCatalogCopies)
	}
}

// TestVerifyChecksTheCatalogCopies: verify skips every generation object, so
// the index that makes a volume readable without a workdir is the one thing
// on it that is never read back.
func TestVerifyChecksTheCatalogCopies(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{TarBelow: -1})
	f.write(map[string]int{"a.bin": 4096})
	p := f.plan()
	f.writeAll(p)
	pv := p.Volumes[0]

	zeroFill(t, filepath.Join(f.shelf, pv.ID, filepath.FromSlash(catalogObjectName(p.Generation, 0))))

	vol, done := f.mount(pv.ID)
	defer done()
	res, err := Verify(context.Background(), vol, f.cat, false)
	if err == nil && !res.Failed() {
		t.Errorf("a volume whose catalog copy is unreadable was reported clean; the index is never checked")
	}
}

// TestARerunAfterProblemsRefreshesTheMediumIndex: a write that finishes with
// problems still places its catalog copies, minus the files it could not
// read. Once the operator fixes the file and reruns the volume, the object is
// written and recorded locally, but both copies are skipped by name, so the
// medium's index never lists an object the cartridge holds.
func TestARerunAfterProblemsRefreshesTheMediumIndex(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything; the permission failure this test needs cannot be produced")
	}
	f := newFixture(t, 4<<20, plan.Layout{TarBelow: -1})
	f.write(map[string]int{"ok.bin": 4096, "locked.bin": 8192})
	p := f.plan()
	pv := p.Volumes[0]
	ctx := context.Background()

	locked := filepath.Join(f.src, "locked.bin")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	vol, done := f.mount(pv.ID)
	res, err := Write(ctx, vol, pv, f.cat, Options{Archive: f.cat.Header.Archive, Generation: p.Generation})
	done()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Problems) != 1 {
		t.Fatalf("expected locked.bin to be reported, got %v", res.Problems)
	}

	// Fixed, and the volume written again — the -volume rerun.
	if err := os.Chmod(locked, 0o644); err != nil {
		t.Fatal(err)
	}
	vol, done = f.mount(pv.ID)
	res, err = Write(ctx, vol, pv, f.cat, Options{Archive: f.cat.Header.Archive, Generation: p.Generation})
	done()
	if err != nil {
		t.Fatal(err)
	}
	if recordFor(res, locked) == nil {
		t.Fatal("the rerun did not write locked.bin")
	}

	vol, done = f.mount(pv.ID)
	defer done()
	copies := readableCopies(t, vol, f.cat.Header.Archive)
	if len(copies) == 0 {
		t.Fatal("the volume carries no readable catalog copy")
	}
	// The highest-numbered copy is the one a recovery would read.
	newest := readCatalogObject(t, vol, copies[len(copies)-1])
	if _, ok := newest.Lookup(locked); !ok {
		t.Errorf("the newest catalog copy on the volume (%s) does not list locked.bin, which the volume holds: the copies were skipped by name", copies[len(copies)-1])
	}
}
