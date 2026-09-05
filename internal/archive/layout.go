// Package archive turns a plan into objects on a volume, and turns those
// objects back into files.
//
// It is the layer between the medium-agnostic core — plan, catalog — and the
// volume backend. Three operations, each handling exactly one volume and then
// returning, because the operator swaps media by hand and the drive may sit
// powered off for weeks in between:
//
//	Write    place one volume's share of a plan, and record what landed where
//	Verify   read every object back and check it against its recorded digest
//	Restore  produce the files one volume holds, refusing to look complete
//	         when it cannot
package archive

import (
	"fmt"
	"path"
	"strings"
)

// Object names on a volume. The three namespaces are kept apart so that a
// source tree containing a file called "kura.volume.json" cannot shadow the
// volume's own metadata.
const (
	// HeaderObject is the one object that stays plaintext even in an encrypted
	// archive: enough to recognise the volume and know how to proceed, and
	// nothing more (DESIGN §3.1).
	HeaderObject = "kura.volume.json"

	// catalogObject is the authoritative index, written to every volume so an
	// archive can be read back with no external database.
	catalogPrefix = "kura.catalog"

	dataPrefix      = "data/"
	containerPrefix = "containers/"
)

// catalogObjectName is the nth copy of the catalog on a volume.
//
// More than one copy is dar's trick (DESIGN §8.1a): the catalog is the one
// structure whose loss makes everything else on the volume unreadable, and it
// is small enough that duplicating it is nearly free.
//
// The copies land next to each other on a sequential medium, which protects
// against a bad block but not against damage to a whole region. Separating them
// properly needs control over position that the volume interface does not
// expose.
func catalogObjectName(n int) string {
	return fmt.Sprintf("%s.%03d.jsonl.gz", catalogPrefix, n)
}

// isCatalogObject reports whether a name is one of the catalog copies, which
// verification checks differently: they are written from memory rather than
// from a source file and have no catalog record of their own.
func isCatalogObject(name string) bool {
	return strings.HasPrefix(name, catalogPrefix+".")
}

// dataObjectName is where a file's bytes live when it is stored as an object of
// its own.
//
// The leading slash is stripped because an object name has to be relative — a
// backend refuses anything that could escape the volume, and those names arrive
// from a catalog that may have been read off media someone else wrote. Keeping
// the rest of the path makes a volume browsable with ordinary tools, which is
// most of the value of the file backend as a real archive rather than a test
// harness.
func dataObjectName(filePath string) string {
	return dataPrefix + relative(filePath)
}

// fragmentObjectName is where one piece of a split file lives. The part number
// is in the name because the pieces sit on different volumes and each is an
// ordinary object on its own.
func fragmentObjectName(filePath string, part int) string {
	return fmt.Sprintf("%s%s.part%03d", dataPrefix, relative(filePath), part)
}

// memberName is what a file is called inside a container. It carries no data/
// prefix: the container is already the namespace.
func memberName(filePath string) string { return relative(filePath) }

// containerObjectName names the tar holding one item's small files.
//
// The generation is part of the name so that a later cycle chunking the same
// directory cannot collide with an earlier one (DESIGN §4.1) — membership is
// fixed when a container is written and never revisited, so new files go into
// new containers rather than shifting old boundaries.
func containerObjectName(gen int, itemName string, part int) string {
	return fmt.Sprintf("%sg%04d/%s.part%03d.tar", containerPrefix, gen, relative(strings.TrimSuffix(itemName, "/")), part)
}

// relative strips leading slashes and refuses anything that would climb out of
// the volume once joined.
func relative(p string) string {
	clean := path.Clean("/" + p)
	return strings.TrimPrefix(clean, "/")
}
