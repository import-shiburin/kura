package plan

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"kura/internal/scan"
)

// checkConservation is the invariant that matters most: every byte handed to
// the packer is placed exactly once, no volume is overfilled, and each volume's
// Used total agrees with what is actually on it.
func checkConservation(t *testing.T, p *Plan, files []*scan.File) {
	t.Helper()

	placed := make(map[string]int64)
	frags := make(map[string][]SplitPart)

	for _, v := range p.Volumes {
		if v.Used > v.Capacity {
			t.Errorf("volume %s overfilled: used %d > capacity %d", v.ID, v.Used, v.Capacity)
		}
		if v.Free() < 0 {
			t.Errorf("volume %s has negative free space: %d", v.ID, v.Free())
		}
		var sum int64
		for _, it := range v.Items {
			var itemSum int64
			for _, f := range it.Files {
				placed[f.Path] += f.Size
				itemSum += f.Size
			}
			if itemSum != it.Size {
				t.Errorf("item %s claims %d bytes but holds %d", it.Name, it.Size, itemSum)
			}
			sum += itemSum
		}
		for _, sp := range v.Splits {
			placed[sp.Path] += sp.Length
			frags[sp.Path] = append(frags[sp.Path], sp)
			sum += sp.Length
		}
		if sum != v.Used {
			t.Errorf("volume %s: Used=%d but contents total %d", v.ID, v.Used, sum)
		}
	}

	want := make(map[string]int64, len(files))
	for _, f := range files {
		want[f.Path] = f.Size
	}
	for path, n := range placed {
		if want[path] != n {
			t.Errorf("%s: placed %d bytes, file is %d", path, n, want[path])
		}
	}
	for path, size := range want {
		if _, ok := placed[path]; !ok && size > 0 {
			t.Errorf("%s (%d bytes) was never placed", path, size)
		}
	}

	// Fragments of a split file must tile [0, size) with no gap or overlap.
	for path, parts := range frags {
		sort.Slice(parts, func(i, j int) bool { return parts[i].Offset < parts[j].Offset })
		var next int64
		for _, sp := range parts {
			if sp.Offset != next {
				t.Errorf("%s: fragment starts at %d, expected %d", path, sp.Offset, next)
			}
			if sp.OfParts != len(parts) {
				t.Errorf("%s: fragment claims %d parts, found %d", path, sp.OfParts, len(parts))
			}
			next += sp.Length
		}
		if next != want[path] {
			t.Errorf("%s: fragments cover %d bytes, file is %d", path, next, want[path])
		}
	}
}

func TestPackConservation(t *testing.T) {
	var files []*scan.File
	for i := range 200 {
		files = append(files, mkfile(fmt.Sprintf("/storage/dir%02d/file%03d", i/20, i), int64(i+1)*7*gb))
	}
	items := Group(files, 200*gb, Layout{})
	p := Pack(items, nil, PackOptions{Capacity: 2500 * gb, MaxBlanks: -1, NamePrefix: "LTO"})
	checkConservation(t, p, files)
	if len(p.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", p.Warnings)
	}
}

func TestPackNeverOverfills(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	var files []*scan.File
	for i := range 3000 {
		files = append(files, mkfile(fmt.Sprintf("/storage/x/f%04d", i), rng.Int63n(60*gb)+1))
	}
	items := Group(files, 200*gb, Layout{})
	p := Pack(items, nil, PackOptions{Capacity: 2500 * gb, Headroom: 25 * gb, MaxBlanks: -1, NamePrefix: "LTO"})
	checkConservation(t, p, files)
	// Usable capacity is the cartridge less the headroom asked for and the room
	// the volume needs to describe itself.
	want := 2500*gb - 25*gb - DefaultMetadataReserve
	for _, v := range p.Volumes {
		if v.Capacity != want {
			t.Errorf("volume %s: usable capacity %d, want %d", v.ID, v.Capacity, want)
		}
		if v.Reserved != 25*gb+DefaultMetadataReserve {
			t.Errorf("volume %s: reserved %d, want headroom plus metadata", v.ID, v.Reserved)
		}
	}
}

// TestPackLeavesRoomForTheVolumesOwnIndex is the bug the metadata reserve
// exists for. A file too large for one volume is split to fill the free space
// exactly, which without a reserve leaves nothing for the header and catalog
// the volume must carry — and the write then fails on its own index.
func TestPackLeavesRoomForTheVolumesOwnIndex(t *testing.T) {
	files := []*scan.File{mkfile("/storage/huge.img", 6*tb)}
	p := Pack(Group(files, 200*gb, Layout{}), nil, PackOptions{
		Capacity: 2500 * gb, MaxBlanks: -1, NamePrefix: "LTO",
		AllowSplit: true, SplitMin: 100 * gb,
	})
	checkConservation(t, p, files)

	for _, v := range p.Volumes {
		if v.Reserved < DefaultMetadataReserve {
			t.Errorf("volume %s reserved %d, less than a volume needs for its own index", v.ID, v.Reserved)
		}
		if v.Capacity+v.Reserved > 2500*gb {
			t.Errorf("volume %s claims more than the cartridge holds", v.ID)
		}
	}
}

// TestFillEfficiency is the measurement behind the claim that packing, rather
// than filling in directory order, is what keeps stranded space negligible.
func TestFillEfficiency(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	var files []*scan.File
	var total int64

	// A size mix in the shape of a media archive: a few very large images, a
	// band of episode-sized files, and a long tail of small ones.
	for i := range 6000 {
		var size int64
		switch r := rng.Float64(); {
		case r < 0.05:
			size = 20*gb + rng.Int63n(25*gb) // disc images
		case r < 0.35:
			size = 800*mb + rng.Int63n(6*gb) // movies, concerts
		case r < 0.70:
			size = 150*mb + rng.Int63n(900*mb) // episodes
		default:
			size = 1 + rng.Int63n(40*mb) // art, subtitles, documents
		}
		files = append(files, mkfile(fmt.Sprintf("/storage/lib%02d/sub%02d/f%05d", i%17, i%7, i), size))
		total += size
	}

	const capacity = 2500 * gb
	items := Group(files, 200*gb, Layout{})
	p := Pack(items, nil, PackOptions{Capacity: capacity, MaxBlanks: -1, NamePrefix: "LTO"})
	checkConservation(t, p, files)

	naiveVolumes, _ := fillInOrder(files, capacity)

	// Stranded space is measured over the closed volumes only. The final one is
	// still appendable next cycle, so counting it as waste would overstate.
	stranded, closedCap := strandedOnClosed(p)
	wastePct := 100 * float64(stranded) / float64(closedCap)
	floor := (total + capacity - 1) / capacity

	t.Logf("payload %s across %d files", FormatBytes(total), len(files))
	t.Logf("theoretical floor: %d volumes", floor)
	t.Logf("FFD:               %d volumes, %s stranded on closed volumes (%.3f%%)",
		len(p.Volumes), FormatBytes(stranded), wastePct)
	t.Logf("fill-in-order:     %d volumes", naiveVolumes)

	if wastePct > 1.0 {
		t.Errorf("stranded space %.3f%% exceeds the 1%% the design assumes", wastePct)
	}
	if int64(len(p.Volumes)) > floor+1 {
		t.Errorf("FFD used %d volumes, floor is %d", len(p.Volumes), floor)
	}
	if naiveVolumes < len(p.Volumes) {
		t.Errorf("fill-in-order beat FFD (%d vs %d); the packer is not doing its job", naiveVolumes, len(p.Volumes))
	}
}

// strandedOnClosed measures unused space across every volume but the last. The
// final one stays appendable next cycle, so counting its free space as waste
// would overstate the loss.
func strandedOnClosed(p *Plan) (stranded, capacity int64) {
	if len(p.Volumes) < 2 {
		return 0, 0
	}
	for _, v := range p.Volumes[:len(p.Volumes)-1] {
		stranded += v.Free()
		capacity += v.Capacity
	}
	return stranded, capacity
}

// fillInOrder is the old behaviour: walk the tree in path order and start a new
// volume the moment a file does not fit.
func fillInOrder(files []*scan.File, capacity int64) (volumes int, stranded int64) {
	ordered := append([]*scan.File(nil), files...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	volumes, free := 1, capacity
	for _, f := range ordered {
		if free < f.Size {
			stranded += free
			volumes++
			free = capacity
		}
		free -= f.Size
	}
	return volumes, stranded
}

// TestFillEfficiencyNoSmallTail is the adversarial case: a library of nothing
// but large, similarly-sized files, with no small file ever left over to top a
// volume off with. This is the shape that makes "20 GiB free and the next file
// is 30 GiB" a real worry.
//
// It also isolates what the -max-group knob costs. Grouping is a locality
// feature, not a packing one: capping groups at 200 GB means the packer is
// fitting 200 GB granules into a 2.5 TB volume, and the remainder of that
// division is pure loss. Packing the same files individually removes it.
func TestFillEfficiencyNoSmallTail(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	var files []*scan.File
	var total int64
	for i := range 700 {
		size := 20*gb + rng.Int63n(25*gb) // 20-45 GB, nothing smaller
		files = append(files, mkfile(fmt.Sprintf("/storage/ISOs/disc%04d.iso", i), size))
		total += size
	}

	const capacity = 2500 * gb
	opts := PackOptions{Capacity: capacity, MaxBlanks: -1, NamePrefix: "LTO"}
	floor := (total + capacity - 1) / capacity

	perFile := Pack(Group(files, 0, Layout{}), nil, opts)
	grouped := Pack(Group(files, 200*gb, Layout{}), nil, opts)
	checkConservation(t, perFile, files)
	checkConservation(t, grouped, files)

	fileStranded, fileCap := strandedOnClosed(perFile)
	groupStranded, groupCap := strandedOnClosed(grouped)
	naiveVolumes, naiveStranded := fillInOrder(files, capacity)

	t.Logf("payload %s across %d files, all 20-45 GB", FormatBytes(total), len(files))
	t.Logf("theoretical floor:      %d volumes", floor)
	t.Logf("FFD, per file:          %d volumes, %s stranded (%.3f%%)",
		len(perFile.Volumes), FormatBytes(fileStranded), 100*float64(fileStranded)/float64(fileCap))
	t.Logf("FFD, 200 GB groups:     %d volumes, %s stranded (%.3f%%)",
		len(grouped.Volumes), FormatBytes(groupStranded), 100*float64(groupStranded)/float64(groupCap))
	t.Logf("fill-in-order per file: %d volumes, %s stranded (%.3f%%)",
		naiveVolumes, FormatBytes(naiveStranded), 100*float64(naiveStranded)/float64(int64(naiveVolumes-1)*capacity))

	if int64(len(perFile.Volumes)) > floor {
		t.Errorf("per-file FFD needed %d volumes, floor is %d", len(perFile.Volumes), floor)
	}
	// Packing files individually is what the packer is for; it must not lose to
	// naive fill on the distribution naive fill handles best.
	if fileStranded > naiveStranded {
		t.Errorf("per-file FFD stranded %s, worse than fill-in-order's %s",
			FormatBytes(fileStranded), FormatBytes(naiveStranded))
	}
	if groupStranded <= fileStranded {
		t.Log("note: grouping cost nothing on this distribution")
	}
}

func TestPackAppendsToExistingVolumes(t *testing.T) {
	files := []*scan.File{
		mkfile("/storage/a", 300*gb),
		mkfile("/storage/b", 900*gb),
	}
	existing := []*Volume{
		{ID: "LTO007", Capacity: 400 * gb, Existing: true},
	}
	items := Group(files, 200*gb, Layout{})
	p := Pack(items, existing, PackOptions{Capacity: 2500 * gb, MaxBlanks: -1, NamePrefix: "LTO", StartIndex: 8})
	checkConservation(t, p, files)

	if p.Volumes[0].ID != "LTO007" || p.Volumes[0].Used != 300*gb {
		t.Errorf("expected the 300 GB file to top off LTO007, got used=%s", FormatBytes(p.Volumes[0].Used))
	}
	if len(p.Volumes) != 2 || p.Volumes[1].ID != "LTO008" {
		t.Errorf("expected one new volume numbered from -start, got %d volumes", len(p.Volumes))
	}
}

func TestSplitDissolvesLastVolume(t *testing.T) {
	// Two volumes' worth plus a single 400 GB file that fits nowhere whole.
	var files []*scan.File
	for i := range 4 {
		files = append(files, mkfile(fmt.Sprintf("/storage/big/f%d", i), 1100*gb))
	}
	files = append(files, mkfile("/storage/big/tail", 400*gb))

	items := Group(files, 200*gb, Layout{})
	opts := PackOptions{Capacity: 2500 * gb, MaxBlanks: -1, NamePrefix: "LTO"}

	whole := Pack(items, nil, opts)
	checkConservation(t, whole, files)

	opts.AllowSplit = true
	opts.SplitMin = 100 * gb
	split := Pack(items, nil, opts)
	checkConservation(t, split, files)

	t.Logf("without splitting: %d volumes; with splitting: %d volumes (saved %d)",
		len(whole.Volumes), len(split.Volumes), split.Summary.VolumesSavedBySplit)

	if len(split.Volumes) >= len(whole.Volumes) {
		t.Errorf("splitting saved nothing: %d vs %d volumes", len(split.Volumes), len(whole.Volumes))
	}
	if split.Summary.VolumesSavedBySplit != len(whole.Volumes)-len(split.Volumes) {
		t.Errorf("VolumesSavedBySplit=%d disagrees with the volume counts", split.Summary.VolumesSavedBySplit)
	}
}

func TestFileLargerThanOneVolume(t *testing.T) {
	files := []*scan.File{mkfile("/storage/huge.img", 6*tb)}
	items := Group(files, 200*gb, Layout{})

	refused := Pack(items, nil, PackOptions{Capacity: 2500 * gb, MaxBlanks: -1, NamePrefix: "LTO"})
	if len(refused.Warnings) == 0 {
		t.Error("expected a warning when a file exceeds one volume and splitting is off")
	}

	spread := Pack(items, nil, PackOptions{
		Capacity: 2500 * gb, MaxBlanks: -1, NamePrefix: "LTO", AllowSplit: true, SplitMin: 100 * gb,
	})
	checkConservation(t, spread, files)
	if len(spread.Volumes) != 3 {
		t.Errorf("6 TB across 2.5 TB volumes should need 3 cartridges, got %d", len(spread.Volumes))
	}
}

func TestPackRespectsVolumeLimit(t *testing.T) {
	var files []*scan.File
	for i := range 10 {
		files = append(files, mkfile(fmt.Sprintf("/storage/f%d", i), 1000*gb))
	}
	items := Group(files, 200*gb, Layout{})
	p := Pack(items, nil, PackOptions{Capacity: 2500 * gb, MaxBlanks: 2, NamePrefix: "LTO"})

	if len(p.Volumes) != 2 {
		t.Errorf("expected the run to stop at 2 volumes, got %d", len(p.Volumes))
	}
	if len(p.Warnings) == 0 {
		t.Error("expected a warning about running out of volumes")
	}
}

func TestPackIsDeterministic(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	var files []*scan.File
	for i := range 500 {
		files = append(files, mkfile(fmt.Sprintf("/storage/d%02d/f%03d", i%9, i), rng.Int63n(80*gb)+1))
	}
	opts := PackOptions{Capacity: 2500 * gb, MaxBlanks: -1, NamePrefix: "LTO", AllowSplit: true, SplitMin: 50 * gb}

	first := Pack(Group(files, 200*gb, Layout{}), nil, opts)
	second := Pack(Group(files, 200*gb, Layout{}), nil, opts)

	if len(first.Volumes) != len(second.Volumes) {
		t.Fatalf("volume count differs between runs: %d vs %d", len(first.Volumes), len(second.Volumes))
	}
	for i := range first.Volumes {
		if first.Volumes[i].Used != second.Volumes[i].Used {
			t.Errorf("volume %d differs between runs: %d vs %d", i, first.Volumes[i].Used, second.Volumes[i].Used)
		}
	}
}
