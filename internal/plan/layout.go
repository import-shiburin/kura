package plan

import (
	"fmt"
	"sort"
	"strings"

	"kura/internal/scan"
)

// Layout says how files are turned into objects: which are small enough to be
// bundled, and how much source data goes into one container.
type Layout struct {
	// Generation is part of every container's name, so a later cycle chunking
	// the same directory cannot collide with this one (DESIGN §4.1).
	Generation int

	// TarBelow is the size under which a file is bundled rather than given an
	// object of its own. Zero or less stores every file separately.
	TarBelow int64

	// MaxContainer caps the source bytes in one container.
	MaxContainer int64
}

// Container is one tar an item will be written as.
type Container struct {
	Name  string
	Files []*scan.File
	Size  int64 // encoded size, framing included
}

const tarBlock = 1024 / 2

// tarTrailer is the two zero blocks that end every tar.
const tarTrailer = 2 * tarBlock

// tarMemberBytes is how much room a file takes inside a container: a header
// block, its content padded to a block boundary, and — for the 78% of real
// paths that exceed tar's 100-byte name field — an extended header carrying the
// real name.
//
// It deliberately over-estimates. Under-estimating means a volume the plan said
// would hold its share does not, and the write stops partway with a cartridge
// half full and an operator standing next to the drive. Over-estimating costs a
// kilobyte of unused tape.
func tarMemberBytes(name string, size int64) int64 {
	n := tarBlock + padTo(size, tarBlock)
	if len(name) > 100 {
		// An extended header block plus one record, "<len> path=<name>\n".
		// The length prefix counts itself, so solving it exactly is a
		// fixed point; 32 bytes of slack is cheaper than the arithmetic.
		n += tarBlock + padTo(int64(len(name))+32, tarBlock)
	}
	return n
}

func padTo(n, block int64) int64 { return (n + block - 1) / block * block }

// bundled reports whether a file is small enough to go into a container.
func (l Layout) bundled(f *scan.File) bool { return l.TarBelow > 0 && f.Size < l.TarBelow }

// storedSize is how much room a file takes on a volume once framing is counted.
//
// Packing has to work in these units rather than source bytes. The difference
// is 1.6% of the tarred data on the real archive, which is nothing against 31
// TB but is the whole of the last container that would not fit.
func (l Layout) storedSize(f *scan.File) int64 {
	if l.bundled(f) {
		return tarMemberBytes(memberName(f.Path), f.Size)
	}
	return f.Size
}

// makeItem builds one packable item, settling its container layout and its
// size on the medium at the same time.
//
// Membership is decided here, once, and never revisited: boundaries fixed at
// this moment are what stop a later cycle's new files from shifting the
// contents of a container already written (DESIGN §4.1).
func (l Layout) makeItem(name string, files []*scan.File, dir bool) Item {
	it := Item{Name: name, Count: len(files), Dir: dir, Files: files}

	var small []*scan.File
	for _, f := range files {
		if l.bundled(f) {
			small = append(small, f)
			continue
		}
		it.Loose = append(it.Loose, f)
		it.Size += f.Size
	}
	if len(small) == 0 {
		return it
	}
	sort.Slice(small, func(i, j int) bool { return small[i].Path < small[j].Path })

	cap := l.MaxContainer
	if cap <= 0 {
		cap = int64(1) << 62
	}
	cur := Container{Name: containerName(l.Generation, name, 1), Size: tarTrailer}
	var source int64
	for _, f := range small {
		if len(cur.Files) > 0 && source+f.Size > cap {
			it.Size += cur.Size
			it.Containers = append(it.Containers, cur)
			cur = Container{Name: containerName(l.Generation, name, len(it.Containers)+1), Size: tarTrailer}
			source = 0
		}
		cur.Files = append(cur.Files, f)
		cur.Size += tarMemberBytes(memberName(f.Path), f.Size)
		source += f.Size
	}
	it.Size += cur.Size
	it.Containers = append(it.Containers, cur)
	return it
}

// containerName is the object name of one container.
func containerName(gen int, itemName string, part int) string {
	trimmed := strings.Trim(itemName, "/")
	if trimmed == "" {
		trimmed = "root"
	}
	return fmt.Sprintf("containers/g%04d/%s.part%03d.tar", gen, trimmed, part)
}

// memberName is what a file is called inside a container: its path without the
// leading slash, so the container unpacks with plain tar into a relative tree.
func memberName(path string) string { return strings.TrimLeft(path, "/") }
