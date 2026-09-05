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
	"strconv"
	"strings"
)

// Object names on a volume. The namespaces are kept apart so that a source tree
// containing a file called "kura.volume.json" cannot shadow the volume's own
// metadata.
const (
	// ArchiveObject holds what is true of the whole archive rather than of this
	// cartridge: its identity, and how its objects are encrypted. Byte-identical
	// on every volume the archive owns (DESIGN §3.1).
	ArchiveObject = "kura.archive.json"

	// HeaderObject holds what is true of this volume alone: which archive it
	// belongs to and which cartridge it is. Both objects stay plaintext even in
	// an encrypted archive, and both are written once and never rewritten.
	//
	// Nothing that changes goes in either of them. On the medium this targets,
	// modifying a file is not modification: LTFS appends the new bytes at the
	// current append point, repoints the index, and abandons the old blocks
	// forever. So a header carrying a "last written" field would cost a fresh
	// copy of itself on every cycle and strand the previous one. Anything
	// mutable is versioned instead — see catalogObjectName.
	HeaderObject = "kura.volume.json"

	// generationPrefix holds everything that belongs to one cycle rather than to
	// the cartridge. The root carries only write-once identity; anything that a
	// later generation would otherwise have had to rewrite lives under here,
	// under its own generation.
	generationPrefix = "generations/"

	dataPrefix      = "data/"
	containerPrefix = "containers/"
)

// catalogObjectName is the nth copy of the catalog as it stood at one
// generation.
//
// More than one copy is dar's trick (DESIGN §8.1a): the catalog is the one
// structure whose loss makes everything else on the volume unreadable, and it
// is small enough that duplicating it is nearly free.
//
// The copies land next to each other on a sequential medium, which protects
// against a bad block but not against damage to a whole region. Separating them
// properly needs control over position that the volume interface does not
// expose.
//
// The generation is in the path for the same reason it is in a container's
// (DESIGN §4.1): a later cycle must not collide with an earlier one. It was
// absent here once, and the copies were simply skipped when the names already
// existed — so a volume appended to at generation 2 kept an index describing
// only generation 1, silently, because nothing reads the on-volume catalog back
// until the day the local one is gone.
func catalogObjectName(gen, n int) string {
	return fmt.Sprintf("%sg%04d/catalog.%03d.jsonl.gz", generationPrefix, gen, n)
}

// isGenerationObject reports whether a name is per-cycle metadata, which
// verification checks differently: those objects are written from memory rather
// than from a source file and have no catalog record of their own.
func isGenerationObject(name string) bool {
	return strings.HasPrefix(name, generationPrefix)
}

// generationOf returns the generation a per-cycle object belongs to, or 0.
//
// Read from the name rather than from the object, which is what lets "how far
// has this cartridge been written?" be answered from a listing. On tape that is
// the difference between consulting the mounted index and reading the medium,
// and in an encrypted archive it is the difference between knowing which
// cartridge is newest and having to unlock every one of them to find out.
func generationOf(name string) int {
	rest, ok := strings.CutPrefix(name, generationPrefix)
	if !ok {
		return 0
	}
	dir, _, ok := strings.Cut(rest, "/")
	if !ok {
		return 0
	}
	digits, ok := strings.CutPrefix(dir, "g")
	if !ok {
		return 0
	}
	gen, err := strconv.Atoi(digits)
	if err != nil || gen < 1 {
		return 0
	}
	return gen
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
