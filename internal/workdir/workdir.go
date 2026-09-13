// Package workdir owns the local state of one archive: the catalog, the plan
// for the current cycle, and the space containers are staged in.
//
// It exists because none of that is the operator's to handle. Naming the
// catalog file on every command leaked kura's own bookkeeping into the
// interface, made the container staging directory a side effect of wherever
// that file happened to sit, and let a plan be paired with the catalog of a
// different archive — a mistake nothing downstream could detect (DESIGN §10.2).
// With one directory per archive, the ordinary invocation names nothing at all
// and the crossed pair stops being constructible by accident.
//
// The medium remains the authority (DESIGN §11). A workdir is a cache: delete
// it and it can be rebuilt from any cartridge the archive owns.
package workdir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"kura/internal/catalog"
)

const (
	// DefaultRoot is where workdirs live when the operator names none. It is a
	// system path rather than one under $HOME because the drive is on a backup
	// host and the cycle is usually run by a service or by root.
	DefaultRoot = "/var/lib/kura"

	// RootEnv overrides DefaultRoot, for a host that keeps state elsewhere or
	// a test that must not touch the real one.
	RootEnv = "KURA_HOME"

	CatalogName = "catalog.jsonl.gz"
	PlanName    = "plan.json"
	StageName   = "stage"

	// ResumeName holds what a deliberately paused write placed on the medium
	// but has not committed to the catalog. Its presence is the only thing that
	// tells a pause from a crash, and it is local on purpose: the medium cannot
	// carry a mutable file, and a marker that goes missing has to fail towards
	// distrust rather than away from it. DESIGN §11.1.
	ResumeName = "resume.jsonl.gz"

	// PauseName is the flag `kura pause` sets and a running write notices
	// between objects. It exists because the write that most needs pausing is
	// the one already running under a unit file, where there is no terminal to
	// send a signal from.
	PauseName = "PAUSE"

	// archiveIDLen is the length of the hex archive ID that
	// archive.NewArchiveID produces: 16 random bytes. Repeated here rather
	// than imported because a workdir only has to recognise the shape of a
	// directory name, and importing the archive package for one constant would
	// pull the whole write path into `history` and `at-gen`.
	archiveIDLen = 32
)

// ErrNotFound reports that no workdir could be resolved. It is distinct from
// every other failure because it is the one a first cycle expects: `plan`
// creates a workdir where the other commands stop.
var ErrNotFound = errors.New("no workdir")

// Root is where workdirs live when none is named.
func Root() string {
	if r := os.Getenv(RootEnv); r != "" {
		return r
	}
	return DefaultRoot
}

// Dir is one archive's local state. The zero value is not usable; get one from
// Resolve or Create.
type Dir struct{ Path string }

// At names a workdir without touching the filesystem.
func At(path string) Dir { return Dir{Path: path} }

func (d Dir) Catalog() string { return filepath.Join(d.Path, CatalogName) }
func (d Dir) Plan() string    { return filepath.Join(d.Path, PlanName) }
func (d Dir) Stage() string   { return filepath.Join(d.Path, StageName) }
func (d Dir) Resume() string  { return filepath.Join(d.Path, ResumeName) }
func (d Dir) Pause() string   { return filepath.Join(d.Path, PauseName) }

func (d Dir) String() string { return d.Path }

// Started reports whether this directory holds an archive's state.
//
// A catalog or a plan is enough. The plan matters: `plan` creates the workdir
// and writes into it a full cycle before the first `write` produces a catalog,
// and a workdir that could not be found again in between would strand the
// cycle it had just laid out.
func (d Dir) Started() bool {
	for _, p := range []string{d.Catalog(), d.Plan()} {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return true
		}
	}
	return false
}

// Archive is the archive this workdir belongs to, or "" when it does not know
// yet.
//
// The catalog is asked first and the directory name second. Both are usually
// available and agree; only one is available at the ends of an archive's life.
// A workdir under the root is named by its archive, which is what gives a
// brand-new archive an identity before any cartridge exists — `plan` has to
// place its files somewhere, and it cannot wait for the first write. An
// operator-named workdir has no such name to read, and stays anonymous until
// its first write mints an ID.
func (d Dir) Archive() string {
	if h, err := catalog.ReadHeader(d.Catalog()); err == nil && h.Archive != "" {
		return h.Archive
	}
	if name := filepath.Base(d.Path); isArchiveID(name) {
		return name
	}
	return ""
}

// Create prepares the directory and its staging area.
//
// Staging is a subdirectory rather than the workdir itself so a half-written
// container cannot be mistaken for state: `Put` needs a size up front, so a
// container is assembled in a local file before being placed, and that file is
// the same shape as the things around it.
func (d Dir) Create() error {
	if err := os.MkdirAll(d.Stage(), 0o700); err != nil {
		return fmt.Errorf("preparing workdir %s: %w", d.Path, err)
	}
	return nil
}

// Resolve finds the workdir to act in.
//
// dir is -workdir and is Root() when empty; archiveID is -archive and is empty
// when the caller named none. The search is the one an operator would do by
// hand, in the order they would do it:
//
//  1. dir itself, when it already holds an archive's state
//  2. dir/<archiveID>, when an archive was named
//  3. the one archive under dir, when there is exactly one
//
// Several archives under the root with no -archive is an error and not a
// choice. One host really does hold more than one — DESIGN §8.1 prefers a
// second archive over erasure coding — and a rule that picked for the operator
// would eventually pick wrong on a command that writes.
func Resolve(dir, archiveID string) (Dir, error) {
	root := abs(dir)
	if root == "" {
		root = abs(Root())
	}

	if d := At(root); d.Started() {
		return d, d.mustBe(archiveID)
	}

	if archiveID != "" {
		d := At(filepath.Join(root, archiveID))
		if !d.Started() {
			return Dir{}, fmt.Errorf("%w for archive %s under %s", ErrNotFound, archiveID, root)
		}
		return d, d.mustBe(archiveID)
	}

	found, err := archivesIn(root)
	if err != nil {
		return Dir{}, err
	}
	switch len(found) {
	case 0:
		return Dir{}, fmt.Errorf("%w under %s", ErrNotFound, root)
	case 1:
		return found[0], nil
	default:
		return Dir{}, fmt.Errorf("%s holds %d archives; name one with -archive:\n  %s",
			root, len(found), strings.Join(names(found), "\n  "))
	}
}

// Create resolves like Resolve and prepares a new workdir when there is none.
//
// newID names a brand-new archive under the root, and is used only when
// nothing existing matched. The caller mints it, because an archive ID is the
// archive package's to make and this package is not going to import the whole
// write path to produce one.
func Create(dir, archiveID, newID string) (Dir, error) {
	d, err := Resolve(dir, archiveID)
	switch {
	case err == nil:
		return d, d.Create()
	case !errors.Is(err, ErrNotFound):
		return Dir{}, err
	}

	// Nothing there yet. An operator who named a directory means that
	// directory, whatever it is called; otherwise the archive names itself,
	// because a workdir under the root has to be told apart from its
	// neighbours and the ID is the only name guaranteed to be unique.
	switch {
	case dir != "":
		d = At(abs(dir))
	case archiveID != "":
		d = At(filepath.Join(abs(Root()), archiveID))
	case newID == "":
		return Dir{}, fmt.Errorf("%w under %s, and no archive ID to make one with", ErrNotFound, Root())
	default:
		d = At(filepath.Join(abs(Root()), newID))
	}
	return d, d.Create()
}

// abs makes a path absolute, because a Dir is recorded in the plan file and
// read back by a command run days later from some other directory. A cwd that
// cannot be resolved is not worth failing over: the relative path still works
// for this process, and a wrong one fails loudly at open.
func abs(path string) string {
	if path == "" {
		return ""
	}
	if a, err := filepath.Abs(path); err == nil {
		return a
	}
	return path
}

// mustBe turns -archive into an assertion, the way -volume is one for verify
// and restore: a script that meant a particular archive is stopped rather than
// pointed at whichever one happened to be there.
func (d Dir) mustBe(archiveID string) error {
	if archiveID == "" {
		return nil
	}
	switch got := d.Archive(); got {
	case archiveID, "":
		return nil
	default:
		return fmt.Errorf("%s holds archive %s, not %s", d.Path, got, archiveID)
	}
}

// archivesIn lists the archive workdirs directly under root, newest name last.
// A directory that is not named like an archive is ignored rather than
// reported: the root is a system path and may hold other things.
func archivesIn(root string) ([]Dir, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var found []Dir
	for _, e := range entries {
		if !e.IsDir() || !isArchiveID(e.Name()) {
			continue
		}
		if d := At(filepath.Join(root, e.Name())); d.Started() {
			found = append(found, d)
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Path < found[j].Path })
	return found, nil
}

func names(dirs []Dir) []string {
	out := make([]string, len(dirs))
	for i, d := range dirs {
		out[i] = d.Path
	}
	return out
}

// isArchiveID reports whether a directory name is an archive ID: lower-case
// hex of exactly the length archive.NewArchiveID emits. Anything else under
// the root belongs to somebody else.
func isArchiveID(s string) bool {
	if len(s) != archiveIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}
