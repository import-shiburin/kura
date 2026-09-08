package archive

import (
	"context"
	"path/filepath"
	"testing"

	"kura/internal/plan"
)

// The two tests here are the archive-side half of the volume-identity hole.
// The other half is in cmd/kura: plan numbers volumes from -start, which
// defaults to 1 on every run, so a second generation names the first one's
// IDs again. Whatever plan does, the writer is where the damage happens, and
// these are the two ways it happens.

// TestAModifiedPathNeverResumesFromAnOlderVersion: when the plan's ID matches
// a written cartridge, Reconcile accepts it as long as the cartridge is not
// ahead — an older generation is the append path. Data object names carry no
// generation, so a file modified since that generation already has an object
// under its name, the writer takes the resume path, and records the old bytes
// under the new generation with the file's current stat data. The plan had
// detected the change; the write discards it, and a restore of generation 2
// hands back generation 1's bytes under a record that says otherwise.
func TestAModifiedPathNeverResumesFromAnOlderVersion(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{TarBelow: -1})
	f.write(map[string]int{"big.bin": 1 << 20, "other.bin": 4096})
	g1 := f.plan()
	ctx := context.Background()

	vol, done := f.mount("drive")
	if _, err := Write(ctx, vol, g1.Volumes[0], f.cat, Options{
		Archive: f.cat.Header.Archive, Generation: g1.Generation,
	}); err != nil {
		t.Fatal(err)
	}
	done()

	path := filepath.Join(f.src, "big.bin")
	rewriteInPlace(t, path, false)
	g2 := f.plan()
	if g2.Volumes[0].ID != g1.Volumes[0].ID {
		t.Fatalf("this test needs generation 2 to reuse generation 1's volume ID, got %s then %s",
			g1.Volumes[0].ID, g2.Volumes[0].ID)
	}
	if len(g2.Volumes[0].Entries) != 1 || g2.Volumes[0].Entries[0].Path != path {
		t.Fatalf("generation 2 should write exactly the modified file, got %+v", g2.Volumes[0].Entries)
	}

	// The generation-1 cartridge is still in the drive.
	vol, done = f.mount("drive")
	_, err := Write(ctx, vol, g2.Volumes[0], f.cat, Options{
		Archive: f.cat.Header.Archive, Generation: g2.Generation,
	})
	done()
	if err != nil {
		return // refusing the cartridge is one right answer
	}
	// Accepting it is only right if the new bytes actually landed.
	rec, ok := f.cat.Lookup(path)
	if !ok || rec.Gen != g2.Generation {
		t.Fatalf("big.bin is not recorded at generation %d", g2.Generation)
	}
	if want := fileDigest(t, path); rec.FullHash != want {
		t.Errorf("generation %d records big.bin with the digest of generation 1's bytes: the object under its name was resumed instead of written, and the edit is never backed up",
			g2.Generation)
	}
}

// TestABlankCartridgeIsNotStampedWithAnIDTheCatalogAlreadyLists: the other
// outcome. A blank cartridge in the drive is stamped with whatever the plan
// names, and nothing checks that name against the inventory the catalog keeps.
// Two cartridges then answer to one ID: verify of either finds the other's
// objects missing and marks every version of those paths lost, so the next
// plan rewrites a cartridge's worth of data and at-gen reports history that
// is physically fine as unavailable.
func TestABlankCartridgeIsNotStampedWithAnIDTheCatalogAlreadyLists(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{TarBelow: -1})
	f.write(map[string]int{"one.bin": 4096})
	g1 := f.plan()
	f.writeAll(g1)

	f.write(map[string]int{"two-more.bin": 8192})
	g2 := f.plan()
	id := g2.Volumes[0].ID
	if id != g1.Volumes[0].ID {
		t.Fatalf("this test needs generation 2 to reuse generation 1's volume ID, got %s then %s", g1.Volumes[0].ID, id)
	}
	listed := false
	for _, v := range f.cat.Header.Volumes {
		listed = listed || v.ID == id
	}
	if !listed {
		t.Fatalf("the catalog should already list %s as written", id)
	}

	vol, done := f.mount("a-blank-cartridge")
	defer done()
	_, err := Write(context.Background(), vol, g2.Volumes[0], f.cat, Options{
		Archive: f.cat.Header.Archive, Generation: g2.Generation,
	})
	if err == nil {
		t.Fatalf("a blank cartridge was stamped as %s while the catalog already lists %s as written; two cartridges now answer to one ID", id, id)
	}
}
