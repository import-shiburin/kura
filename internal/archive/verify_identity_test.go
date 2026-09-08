package archive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"kura/internal/catalog"
	"kura/internal/plan"
)

// twoArchives writes one cartridge for each of two archives whose first volume
// carries the same ID, which is what the default prefix produces on any host
// holding two archives — the arrangement DESIGN §8.1 recommends.
func twoArchives(t *testing.T) (ours, theirs *fixture, id string) {
	t.Helper()
	ours = newFixture(t, 4<<20, plan.Layout{TarBelow: -1})
	ours.write(map[string]int{"a.bin": 4096})
	pa := ours.plan()
	ours.writeAll(pa)

	theirs = newFixture(t, 4<<20, plan.Layout{TarBelow: -1})
	theirs.cat.Header.Archive = "somebody-elses-archive"
	theirs.write(map[string]int{"b.bin": 4096})
	pb := theirs.plan()
	theirs.writeAll(pb)

	if pa.Volumes[0].ID != pb.Volumes[0].ID {
		t.Fatalf("this test needs both archives to name their first cartridge alike, got %s and %s",
			pa.Volumes[0].ID, pb.Volumes[0].ID)
	}
	return ours, theirs, pa.Volumes[0].ID
}

// TestVerifyRefusesACartridgeFromAnotherArchive: write reads the archive
// header first, so a foreign cartridge is refused before anything else is
// decided. verify reads only kura.volume.json. With colliding IDs it takes the
// other archive's cartridge for ours, finds every one of our records for that
// ID missing, and marks them lost — after which the next plan rewrites a
// cartridge's worth of data and at-gen reports intact history as unavailable.
func TestVerifyRefusesACartridgeFromAnotherArchive(t *testing.T) {
	ours, theirs, id := twoArchives(t)

	vol, done := theirs.mount(id)
	res, err := Verify(context.Background(), vol, ours.cat, true)
	done()
	if err == nil {
		t.Fatalf("verify accepted another archive's cartridge as %s and reported %d findings against our records", res.Volume, len(res.Findings))
	}
	if !errors.Is(err, ErrForeign) {
		t.Errorf("the refusal should say the cartridge is foreign: %v", err)
	}
	for _, rec := range ours.cat.Records {
		if rec.IsLost() {
			t.Errorf("%s was marked lost by a scrub of a cartridge that never held it", rec.Path)
		}
	}
}

// TestRestoreRefusesACartridgeFromAnotherArchive is the same check for the
// other read path.
func TestRestoreRefusesACartridgeFromAnotherArchive(t *testing.T) {
	ours, theirs, id := twoArchives(t)
	dest := t.TempDir()

	vol, done := theirs.mount(id)
	_, err := Restore(context.Background(), vol, ours.cat, 1, dest, true)
	done()
	if err == nil {
		t.Fatal("restore accepted another archive's cartridge")
	}
	if !errors.Is(err, ErrForeign) {
		t.Errorf("the refusal should say the cartridge is foreign: %v", err)
	}
}

// TestVerifyDoesNotStampAVolumeItCouldNotCheck: a catalog that holds no
// records for the loaded volume — stale, rebuilt, or the wrong archive's —
// checks nothing, reports Clean, and stamps LastVerified. A scrub that read no
// object has verified nothing, and the inventory must not say otherwise.
func TestVerifyDoesNotStampAVolumeItCouldNotCheck(t *testing.T) {
	ours := newFixture(t, 4<<20, plan.Layout{TarBelow: -1})
	ours.write(map[string]int{"a.bin": 4096})
	p := ours.plan()
	ours.writeAll(p)
	id := p.Volumes[0].ID

	stale := catalog.New()
	stale.Header.Archive = ours.cat.Header.Archive

	vol, done := ours.mount(id)
	res, err := Verify(context.Background(), vol, stale, true)
	done()
	if err == nil && !res.Failed() {
		t.Errorf("a volume the catalog holds no records for was reported clean after checking %d objects", res.Checked)
	}
	for _, v := range stale.Header.Volumes {
		if v.ID == id && v.LastVerified != "" {
			t.Errorf("LastVerified was stamped on a volume nothing was checked on")
		}
	}
}

// TestVerifyMarksOnlyTheDamagedVersion: DESIGN §5.1 makes loss a property of
// the version, not the path, so that damage to one cartridge costs only the
// generations it held. markLost marks every record of the path on the volume,
// so an intact older version inside a container is marked with a damaged
// newer object beside it, and a restore of the older generation refuses a
// file it could produce.
func TestVerifyMarksOnlyTheDamagedVersion(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{TarBelow: 1 << 16})
	f.write(map[string]int{"grow.bin": 900, "other.txt": 800})
	f.writeAll(f.plan())

	// Grown past the threshold: an object of its own this time, on the same
	// cartridge as the container holding the first version.
	f.write(map[string]int{"grow.bin": 1 << 20})
	f.writeAll(f.plan())

	path := filepath.Join(f.src, "grow.bin")
	versions := f.cat.Versions(path)
	if len(versions) != 2 {
		t.Fatalf("expected two versions of grow.bin, got %d", len(versions))
	}
	v1, v2 := versions[0], versions[1]
	if v1.Volume != v2.Volume {
		t.Fatalf("this test needs both versions on one cartridge, got %s and %s", v1.Volume, v2.Volume)
	}
	if !v1.Inside() || v2.Inside() {
		t.Fatalf("expected the first version in a container and the second as an object, got container=%q/%q", v1.Container, v2.Container)
	}

	target := objectPath(f, v2.Volume, v2)
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	b[100] ^= 0xFF
	if err := os.WriteFile(target, b, 0o644); err != nil {
		t.Fatal(err)
	}

	vol, done := f.mount(v2.Volume)
	res, err := Verify(context.Background(), vol, f.cat, true)
	done()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("expected one finding, got %v", res.Findings)
	}
	if !v2.IsLost() {
		t.Error("the damaged version was not marked lost")
	}
	if v1.IsLost() {
		t.Error("the generation-1 version is intact inside its container and was marked lost with the generation-2 object: " +
			"loss is per version, and a restore of generation 1 now refuses a file it could produce")
	}
}
