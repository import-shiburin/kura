package archive

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kura/internal/catalog"
	"kura/internal/plan"
	"kura/internal/scan"
)

// rewriteInPlace replaces a file's bytes with different ones of the same
// length. With keepMtime the mtime is put back afterwards, leaving ctime as the
// only stat-level trace — the case DESIGN §6 exists for; otherwise the mtime is
// moved on so the change is visible to a size-and-mtime check.
func rewriteInPlace(t *testing.T, path string, keepMtime bool) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range b {
		b[i] = ^b[i]
	}
	if err := os.WriteFile(path, b, st.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	mtime := st.ModTime()
	if !keepMtime {
		mtime = mtime.Add(2 * time.Second)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// zeroFill is what a power cut can leave behind: an object renamed into place
// at its full size whose contents never reached the platter.
func zeroFill(t *testing.T, path string) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, st.Size()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	d, err := scan.FullHash(path)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// objectPath is where a record's bytes sit on the shelf.
func objectPath(f *fixture, slot string, rec *catalog.Record) string {
	return filepath.Join(f.shelf, slot, filepath.FromSlash(rec.ReadFrom()))
}

// placeWithoutCatalog stands in for a run killed after its objects landed and
// before the local catalog was saved: the objects are on the medium, the
// catalog under test knows nothing. It returns what that run placed.
func placeWithoutCatalog(t *testing.T, f *fixture, pv *plan.Volume, gen int) *Result {
	t.Helper()
	scratch := catalog.New()
	scratch.Header.Archive = f.cat.Header.Archive
	vol, done := f.mount(pv.ID)
	defer done()
	res, err := Write(context.Background(), vol, pv, scratch, Options{
		Archive: f.cat.Header.Archive, Generation: gen,
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func recordFor(res *Result, path string) *catalog.Record {
	for _, r := range res.Records {
		if r.Path == path {
			return r
		}
	}
	return nil
}

func reportedAsProblem(res *Result, path string) bool {
	for _, p := range res.Problems {
		if p.Path == path {
			return true
		}
	}
	return false
}

// TestResumeDoesNotAdoptASourceEditedSinceThePlan documents the resume path
// trusting the medium over the source.
//
// resumeObject and resumeContainer re-digest the object already on the volume
// and build the record from that digest plus the source file's *current* stat
// data. A file edited at the same size between the interruption and the
// resume is therefore recorded as protected — with the old bytes' digest and
// the new file's ctime and inode — so the next cycle settles it as unchanged
// and the edit is never backed up. A restore returns the old bytes under a
// record that claims the new ones.
func TestResumeDoesNotAdoptASourceEditedSinceThePlan(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{TarBelow: 1 << 16})
	f.write(map[string]int{"big.bin": 1 << 20, "docs/a.md": 900, "docs/b.md": 950})
	p := f.plan()
	pv := p.Volumes[0]
	placeWithoutCatalog(t, f, pv, p.Generation)

	// One object of its own, one container member.
	edited := []string{filepath.Join(f.src, "big.bin"), filepath.Join(f.src, "docs/a.md")}
	for _, path := range edited {
		rewriteInPlace(t, path, false)
	}

	vol, done := f.mount(pv.ID)
	res, err := Write(context.Background(), vol, pv, f.cat, Options{
		Archive: f.cat.Header.Archive, Generation: p.Generation,
	})
	done()
	if err != nil {
		t.Fatalf("a source that changed under a resumed run is a per-file problem, not a failed volume: %v", err)
	}
	for _, path := range edited {
		rec := recordFor(res, path)
		switch {
		case rec == nil && !reportedAsProblem(res, path):
			t.Errorf("%s was neither recorded nor reported", path)
		case rec != nil && rec.FullHash != fileDigest(t, path):
			t.Errorf("%s was recorded with the digest of the object already on the medium rather than of the file it claims to describe; "+
				"the next cycle will call it unchanged and the edit is never backed up", path)
		}
	}
}

// TestResumeDoesNotAdoptAnObjectThatDidNotSurviveTheCrash is the same trust,
// met from the other side. Put deliberately does not fsync, so a power cut
// before the end-of-volume Sync can leave an object renamed into place at its
// full size and zero-filled. The resume digests the zeros and records them,
// after which verify compares the object against a digest of its own damage
// and can never disagree. CLAUDE.md's "damage the mandatory verify pass
// catches anyway" is false on this path.
func TestResumeDoesNotAdoptAnObjectThatDidNotSurviveTheCrash(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{TarBelow: 1 << 16})
	f.write(map[string]int{"big.bin": 1 << 20, "docs/a.md": 900, "docs/b.md": 950})
	p := f.plan()
	pv := p.Volumes[0]
	placed := placeWithoutCatalog(t, f, pv, p.Generation)

	// Every object the killed run placed — the data object and the container —
	// is now the right size and all zeros.
	var damaged []string
	for _, rec := range placed.Records {
		damaged = append(damaged, rec.Path)
		zeroFill(t, objectPath(f, pv.ID, rec))
	}

	vol, done := f.mount(pv.ID)
	res, err := Write(context.Background(), vol, pv, f.cat, Options{
		Archive: f.cat.Header.Archive, Generation: p.Generation,
	})
	done()
	if err != nil {
		t.Fatalf("an object that did not survive the crash is a per-file problem, not a failed volume: %v", err)
	}
	for _, path := range damaged {
		rec := recordFor(res, path)
		switch {
		case rec == nil && !reportedAsProblem(res, path):
			t.Errorf("%s was neither recorded nor reported", path)
		case rec != nil && rec.FullHash != fileDigest(t, path):
			t.Errorf("%s was recorded from a zero-filled object: verify will check the object against a digest of the damage and call it clean", path)
		}
	}

	// And nothing zero-filled reaches a restore under its real name.
	dest, _ := f.restoreAll(p, p.Generation)
	root := filepath.Join(dest, strings.TrimPrefix(f.src, "/"))
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		got, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		want, err := os.ReadFile(filepath.Join(f.src, rel))
		if err != nil {
			return err
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s was restored with bytes that are not the source's", rel)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
