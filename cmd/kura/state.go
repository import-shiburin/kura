package main

import (
	"flag"
	"fmt"

	"kura/internal/archive"
	"kura/internal/workdir"
)

// stateFlags are the two flags that say which archive a command acts on.
//
// They replaced -catalog, which named kura's own bookkeeping file on every
// command. That was wrong three ways: it made the operator responsible for a
// file they should never have to touch, it made the container staging
// directory a side effect of wherever that file happened to sit, and it let a
// plan be paired with another archive's catalog. DESIGN §10.3.
type stateFlags struct {
	dir     *string
	archive *string
}

func registerState(fs *flag.FlagSet) *stateFlags {
	return &stateFlags{
		dir: fs.String("workdir", "", fmt.Sprintf(
			"this archive's local state; default is the one archive under %s", workdir.Root())),
		archive: fs.String("archive", "", "refuse to act on any archive but this one"),
	}
}

// open resolves the workdir a command acts in and stops when there is none.
// Every command but plan needs one that already exists.
func (s *stateFlags) open() workdir.Dir {
	d, err := workdir.Resolve(*s.dir, *s.archive)
	if err != nil {
		fatal(err)
	}
	return d
}

// create resolves the workdir, making one when this is a first cycle.
//
// Only plan calls it, and only plan can: a workdir under the root is named by
// its archive, so a brand-new archive has to be given an identity here rather
// than at its first write. That is a change from where the ID used to be
// minted, and it makes the interrupted-first-write path stronger rather than
// weaker — the ID reaches the plan file before any cartridge is touched, and
// ensureArchiveID still prefers whatever the medium says over it (DESIGN §11).
//
// The ID is minted even when an existing workdir turns out to match, and
// thrown away in that case. Sixteen bytes from crypto/rand is not worth a
// callback to avoid.
func (s *stateFlags) create() workdir.Dir {
	id, err := archive.NewArchiveID()
	if err != nil {
		fatal(err)
	}
	d, err := workdir.Create(*s.dir, *s.archive, id)
	if err != nil {
		fatal(err)
	}
	return d
}
