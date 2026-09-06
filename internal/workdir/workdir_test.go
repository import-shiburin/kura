package workdir

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kura/internal/catalog"
)

const (
	idA = "0123456789abcdef0123456789abcdef"
	idB = "fedcba9876543210fedcba9876543210"
)

// rootAt points Root() at a temp directory. Every test needs it: the real root
// is /var/lib/kura, and a test that resolved against the host's own archives
// would be reading somebody's backup state.
func rootAt(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(RootEnv, dir)
	return dir
}

// started makes a directory look like a workdir, by the marker named.
func started(t *testing.T, path, marker string) Dir {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, marker), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return At(path)
}

// withCatalog writes a real catalog, so Archive() has a header to read.
func withCatalog(t *testing.T, path, archiveID string) Dir {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	d := At(path)
	h := catalog.Header{Format: catalog.Format, Archive: archiveID, Generation: 1}
	if err := catalog.Write(d.Catalog(), h, nil); err != nil {
		t.Fatal(err)
	}
	return d
}

// TestTheOnlyArchiveNeedsNoNaming is the whole point of the change: on a host
// with one archive — which is the real case — the operator names nothing, and
// every command finds the same state.
func TestTheOnlyArchiveNeedsNoNaming(t *testing.T) {
	root := rootAt(t)
	want := started(t, filepath.Join(root, idA), CatalogName)

	got, err := Resolve("", "")
	if err != nil {
		t.Fatalf("the one archive under the root was not found: %v", err)
	}
	if got.Path != want.Path {
		t.Errorf("resolved %s, wanted %s", got.Path, want.Path)
	}
}

// TestTwoArchivesRefuseToBeGuessedBetween. One host really does hold more than
// one — DESIGN §8.1 prefers a second archive over erasure coding — and a rule
// that picked for the operator would eventually pick wrong on a command that
// writes 2.5 TB to a cartridge.
func TestTwoArchivesRefuseToBeGuessedBetween(t *testing.T) {
	root := rootAt(t)
	started(t, filepath.Join(root, idA), CatalogName)
	started(t, filepath.Join(root, idB), CatalogName)

	_, err := Resolve("", "")
	if err == nil {
		t.Fatal("one of two archives was picked without being asked for")
	}
	// The error has to be actionable: both candidates named, and the flag that
	// resolves it.
	for _, want := range []string{idA, idB, "-archive"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must mention %q: %v", want, err)
		}
	}
}

// TestArchiveIDSelectsBetweenThem is the other half: naming one is enough, and
// no path has to be typed.
func TestArchiveIDSelectsBetweenThem(t *testing.T) {
	root := rootAt(t)
	started(t, filepath.Join(root, idA), CatalogName)
	started(t, filepath.Join(root, idB), CatalogName)

	got, err := Resolve("", idB)
	if err != nil {
		t.Fatalf("naming an archive did not select it: %v", err)
	}
	if got.Path != filepath.Join(root, idB) {
		t.Errorf("resolved %s, wanted the %s workdir", got.Path, idB)
	}
}

// TestWorkdirIsTriedBeforeLookingInsideIt: a directory that is itself a
// workdir wins over a search for children, which is what lets -workdir name an
// archive that does not live under the root at all.
func TestWorkdirIsTriedBeforeLookingInsideIt(t *testing.T) {
	dir := t.TempDir()
	started(t, dir, CatalogName)
	started(t, filepath.Join(dir, idA), CatalogName)

	got, err := Resolve(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != dir {
		t.Errorf("resolved %s, wanted the named directory %s", got.Path, dir)
	}
}

// TestArchiveIDFindsAWorkdirUnderTheNamedDirectory: given a directory that is
// not itself a workdir, the archive names the child to descend into.
func TestArchiveIDFindsAWorkdirUnderTheNamedDirectory(t *testing.T) {
	dir := t.TempDir()
	started(t, filepath.Join(dir, idA), CatalogName)

	got, err := Resolve(dir, idA)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != filepath.Join(dir, idA) {
		t.Errorf("resolved %s, wanted %s", got.Path, filepath.Join(dir, idA))
	}
}

// TestArchiveIDIsAnAssertion. Mirrors -volume for verify and restore: a script
// that meant a particular archive is stopped rather than pointed at whichever
// one the path happened to hold.
func TestArchiveIDIsAnAssertion(t *testing.T) {
	dir := t.TempDir()
	withCatalog(t, dir, idA)

	_, err := Resolve(dir, idB)
	if err == nil {
		t.Fatal("a workdir holding one archive was accepted as another")
	}
	if !strings.Contains(err.Error(), idA) || !strings.Contains(err.Error(), idB) {
		t.Errorf("the refusal must name both archives: %v", err)
	}
}

// TestAPlanAloneMakesAWorkdir. `plan` creates the workdir and writes into it a
// whole cycle before the first `write` produces a catalog. A workdir that could
// not be found again in that window would strand the cycle it had just laid
// out — days of scanning, on a plan nothing could pick up.
func TestAPlanAloneMakesAWorkdir(t *testing.T) {
	root := rootAt(t)
	started(t, filepath.Join(root, idA), PlanName)

	got, err := Resolve("", "")
	if err != nil {
		t.Fatalf("a workdir holding only a plan was not found: %v", err)
	}
	if got.Path != filepath.Join(root, idA) {
		t.Errorf("resolved %s, wanted %s", got.Path, filepath.Join(root, idA))
	}
}

// TestNothingAnywhereIsErrNotFound, and specifically that sentinel: it is how
// plan tells a first cycle apart from a broken one, and creates rather than
// stops.
func TestNothingAnywhereIsErrNotFound(t *testing.T) {
	rootAt(t)
	if _, err := Resolve("", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an empty root gave %v, wanted ErrNotFound", err)
	}
}

// TestCreateNamesANewArchiveAfterItself. The workdir under the root is named by
// its archive, and that is what gives a brand-new archive an identity before
// any cartridge exists: plan has to put its files somewhere and cannot wait for
// the first write to mint one.
func TestCreateNamesANewArchiveAfterItself(t *testing.T) {
	root := rootAt(t)

	d, err := Create("", "", idA)
	if err != nil {
		t.Fatal(err)
	}
	if d.Path != filepath.Join(root, idA) {
		t.Errorf("a new archive landed in %s, wanted %s", d.Path, filepath.Join(root, idA))
	}
	if d.Archive() != idA {
		t.Errorf("the new workdir does not know its archive: %q", d.Archive())
	}
	if fi, err := os.Stat(d.Stage()); err != nil || !fi.IsDir() {
		t.Errorf("staging was not prepared: %v", err)
	}
}

// TestCreateFindsAnExistingArchiveInsteadOfMakingAnother: every cycle after the
// first runs the same command, and a second workdir per cycle would leave the
// archive's history split across directories.
func TestCreateFindsAnExistingArchiveInsteadOfMakingAnother(t *testing.T) {
	root := rootAt(t)
	started(t, filepath.Join(root, idA), CatalogName)

	d, err := Create("", "", idB)
	if err != nil {
		t.Fatal(err)
	}
	if d.Path != filepath.Join(root, idA) {
		t.Errorf("a second cycle made a new workdir %s instead of reusing %s", d.Path, idA)
	}
}

// TestCreateHonoursANamedDirectoryVerbatim: an operator who names a path means
// that path, and does not want an archive-ID directory appearing inside it.
func TestCreateHonoursANamedDirectoryVerbatim(t *testing.T) {
	rootAt(t)
	dir := filepath.Join(t.TempDir(), "photos")

	d, err := Create(dir, "", idA)
	if err != nil {
		t.Fatal(err)
	}
	if d.Path != dir {
		t.Errorf("a named workdir became %s", d.Path)
	}
	// It has no archive-shaped name to read, and no catalog yet, so it stays
	// anonymous until its first write mints an ID.
	if got := d.Archive(); got != "" {
		t.Errorf("a fresh named workdir claimed archive %q", got)
	}
}

// TestTheCatalogOutranksTheDirectoryName. Both are usually available and agree;
// when they cannot, the catalog is the one written by a real cycle and the name
// is only a label.
func TestTheCatalogOutranksTheDirectoryName(t *testing.T) {
	root := rootAt(t)
	d := withCatalog(t, filepath.Join(root, idA), idB)

	if got := d.Archive(); got != idB {
		t.Errorf("Archive() answered %q from the directory name; the catalog says %q", got, idB)
	}
}

// TestNonArchiveDirectoriesUnderTheRootAreIgnored: the root is a system path
// and may hold other things. Anything not named like an archive belongs to
// somebody else.
func TestNonArchiveDirectoriesUnderTheRootAreIgnored(t *testing.T) {
	root := rootAt(t)
	started(t, filepath.Join(root, idA), CatalogName)
	started(t, filepath.Join(root, "notes"), CatalogName)
	started(t, filepath.Join(root, strings.ToUpper(idB)), CatalogName) // hex, but not lower-case

	got, err := Resolve("", "")
	if err != nil {
		t.Fatalf("neighbours in the root made the one archive ambiguous: %v", err)
	}
	if got.Path != filepath.Join(root, idA) {
		t.Errorf("resolved %s, wanted %s", got.Path, filepath.Join(root, idA))
	}
}

// TestPathsAreAbsolute, because a Dir is recorded in the plan file and read
// back by a command run days later from some other directory.
func TestPathsAreAbsolute(t *testing.T) {
	root := rootAt(t)
	started(t, filepath.Join(root, idA), CatalogName)
	t.Chdir(root) // restored afterwards; os.Chdir here would leak into every later test

	got, err := Resolve(idA, "")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got.Path) {
		t.Errorf("a relative -workdir stayed relative: %s", got.Path)
	}
}

// TestRootFallsBackToTheSystemPath when the environment says nothing, because
// the drive is on a backup host and the cycle is run by a service or by root.
func TestRootFallsBackToTheSystemPath(t *testing.T) {
	t.Setenv(RootEnv, "")
	if got := Root(); got != DefaultRoot {
		t.Errorf("Root() is %q with no %s set, wanted %q", got, RootEnv, DefaultRoot)
	}
}
