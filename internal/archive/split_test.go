package archive

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kura/internal/plan"
	"kura/internal/scan"
)

// A split file is the one kind of file the writer only ever sees a piece of at
// a time, days apart, and the tests here are the three things that follow
// from not treating those pieces as one file.

func splitFixture(t *testing.T) (*fixture, *plan.Plan, string) {
	t.Helper()
	f := newFixture(t, 1<<20, plan.Layout{TarBelow: -1})
	f.write(map[string]int{"huge.bin": (2 << 20) + 12345})
	p := f.plan(func(o *plan.PackOptions) {
		o.AllowSplit = true
		o.SplitMin = 1
	})
	if len(p.Volumes) < 3 {
		t.Fatalf("expected the file to span at least 3 volumes, got %d", len(p.Volumes))
	}
	return f, p, filepath.Join(f.src, "huge.bin")
}

// TestASplitFileIsNotAssembledFromTwoVersions: fragment() opens the file,
// seeks and copies its range, and never compares the file with what the plan
// described — openSource does that for whole objects and containers, and
// fragments skip it. Cartridges of one split file are written days apart, so
// a file rewritten at the same size in between lands as old bytes on one
// cartridge and new bytes on the next. Each part digests only itself, every
// verify is clean, and the restore reports OK on a file that equals neither
// version.
func TestASplitFileIsNotAssembledFromTwoVersions(t *testing.T) {
	f, p, path := splitFixture(t)
	ctx := context.Background()
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	first := p.Volumes[0]
	vol, done := f.mount(first.ID)
	if _, err := Write(ctx, vol, first, f.cat, Options{Archive: f.cat.Header.Archive, Generation: p.Generation}); err != nil {
		t.Fatal(err)
	}
	done()

	// Rewritten between cartridges, mtime put back.
	rewriteInPlace(t, path, true)
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	for _, pv := range p.Volumes[1:] {
		vol, done := f.mount(pv.ID)
		_, err := Write(ctx, vol, pv, f.cat, Options{Archive: f.cat.Header.Archive, Generation: p.Generation})
		done()
		if err != nil {
			t.Logf("%s refused: %v", pv.ID, err) // one right answer; the restore below settles the other
		}
	}

	dest, results := f.restoreAll(p, p.Generation)
	restored := filepath.Join(dest, strings.TrimPrefix(f.src, "/"), "huge.bin")
	got, err := os.ReadFile(restored)
	if err != nil {
		// Not produced, which is fine as long as some mount said so.
		for _, r := range results {
			if !r.OK() || len(r.Incomplete) > 0 {
				return
			}
		}
		t.Fatalf("huge.bin was not restored and no volume reported it")
	}
	if !bytes.Equal(got, original) && !bytes.Equal(got, current) {
		t.Fatalf("huge.bin was restored as a file that is neither the version planned nor the version on disk: fragments of two versions were stitched together and every per-part digest agreed")
	}
}

// TestASplitRecordCarriesWhatChangeDetectionNeeds: fragment() builds its record
// from the plan entry alone — path, size, mtime, generation, parts — where
// record() stats the file and fills in ctime, inode and mode. The largest
// files in the archive, the ones where a rewrite costs most, are therefore the
// only ones that fall back to mtime alone, and a restore cannot give them
// their mode back.
func TestASplitRecordCarriesWhatChangeDetectionNeeds(t *testing.T) {
	f, p, path := splitFixture(t)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	f.writeAll(p)

	rec, ok := f.cat.Lookup(path)
	if !ok {
		t.Fatal("huge.bin was not recorded")
	}
	if rec.CTime == 0 {
		t.Error("the split record has no ctime, so a rewrite behind a restored mtime can never make it suspect")
	}
	if rec.Inode == 0 {
		t.Error("the split record has no inode")
	}
	if rec.Mode == 0 {
		t.Error("the split record has no mode, so a restore cannot reproduce the file's permissions")
	}
}

// TestASplitFileRewrittenBehindItsMtimeIsCaught is what the missing ctime
// costs, measured at the scan: the same in-place edit that a whole file's
// record catches ("genuinely changed") is settled as unchanged for a split
// file, and the edit is never backed up.
func TestASplitFileRewrittenBehindItsMtimeIsCaught(t *testing.T) {
	f, p, path := splitFixture(t)
	f.writeAll(p)

	rewriteInPlace(t, path, true)

	files, _, err := scan.Walk([]scan.Root{{Scan: f.src}}, scan.NewExcluder(nil, true), func(string, error) {})
	if err != nil {
		t.Fatal(err)
	}
	d := scan.Diff(files, f.cat, scan.Options{Match: scan.MatchSample, Jobs: 1}, func(string, error) {})
	if d.UnchangedFiles != 0 || len(d.Modified) != 1 {
		t.Errorf("a split file rewritten behind a restored mtime was settled as unchanged (unchanged=%d modified=%d): "+
			"its record carries no ctime, so the check DESIGN §6 exists for never runs", d.UnchangedFiles, len(d.Modified))
	}
}
