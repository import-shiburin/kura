package archive

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kura/internal/plan"
)

// TestRestoreNeverWritesOutsideTheDestination: object names are treated as
// untrusted input everywhere — relative() and filevol.resolve refuse anything
// that climbs out of a volume, because a catalog may have been read off a
// cartridge someone else wrote. Paths get no such treatment: destPath trims the
// leading slash and joins, so a record whose path starts with ../ is written
// above -into. The same catalog that names the objects names the paths.
func TestRestoreNeverWritesOutsideTheDestination(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{TarBelow: 1 << 16})
	f.write(map[string]int{"big.bin": 1 << 20, "small.txt": 900})
	p := f.plan()
	f.writeAll(p)

	big, ok := f.cat.Lookup(filepath.Join(f.src, "big.bin"))
	if !ok {
		t.Fatal("big.bin was not recorded")
	}
	small, ok := f.cat.Lookup(filepath.Join(f.src, "small.txt"))
	if !ok {
		t.Fatal("small.txt was not recorded")
	}
	// Two records that claim honest bytes under paths that climb out of any
	// destination: one an object of its own, one a container member.
	escapedObject := *big
	escapedObject.Path = "/../../escaped-object.bin"
	escapedMember := *small
	escapedMember.Path = "/../../escaped-member.txt"
	f.cat.Add(&escapedObject, &escapedMember)

	base := t.TempDir()
	dest := filepath.Join(base, "deep", "inner")
	vol, done := f.mount(p.Volumes[0].ID)
	res, err := Restore(context.Background(), vol, f.cat, p.Generation, dest, true)
	done()
	if err != nil {
		t.Fatal(err)
	}

	err = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !strings.HasPrefix(path, dest+string(filepath.Separator)) {
			t.Errorf("restore wrote %s outside the destination", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	refused := map[string]bool{}
	for _, fnd := range res.Failed {
		refused[fnd.Path] = true
	}
	for _, evil := range []string{escapedObject.Path, escapedMember.Path} {
		if !refused[evil] {
			t.Errorf("%s was not reported as refused; a restore that cannot produce a record must say so", evil)
		}
	}
	// The honest files still come back.
	for _, rel := range []string{"big.bin", "small.txt"} {
		if _, err := os.Stat(filepath.Join(dest, strings.TrimPrefix(f.src, "/"), rel)); err != nil {
			t.Errorf("%s was not restored: %v", rel, err)
		}
	}
}
