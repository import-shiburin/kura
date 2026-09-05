package plan

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"kura/internal/scan"
)

const tb = int64(1e12)
const gb = int64(1e9)
const mb = int64(1e6)

func mkfile(path string, size int64) *scan.File {
	return &scan.File{Path: path, Size: size, Mode: 0o644}
}

func names(items []Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = fmt.Sprintf("%s(%s)", it.Name, FormatBytes(it.Size))
	}
	return out
}

// TestGroupKeepsSmallTreesTogether is the locality guarantee: a directory that
// fits comfortably should not be scattered across volumes, because that is what
// makes a restore need three mounts instead of one.
func TestGroupKeepsSmallTreesTogether(t *testing.T) {
	var files []*scan.File
	for i := range 20 {
		files = append(files, mkfile(fmt.Sprintf("/storage/Show/Season 01/ep%02d.mkv", i), 2*gb))
	}
	items := Group(files, 200*gb, Layout{})

	if len(items) != 1 {
		t.Fatalf("expected one atomic item for a 40 GB season, got %d: %v", len(items), names(items))
	}
	if items[0].Count != 20 || items[0].Size != 40*gb {
		t.Errorf("item holds %d files / %s, want 20 / 40 GB", items[0].Count, FormatBytes(items[0].Size))
	}
	if !items[0].Dir {
		t.Error("a whole-directory item should be marked as a directory")
	}
}

func TestGroupSplitsOversizedTrees(t *testing.T) {
	var files []*scan.File
	for season := range 5 {
		for ep := range 30 {
			files = append(files, mkfile(fmt.Sprintf("/storage/Big/S%02d/ep%02d.mkv", season, ep), 4*gb))
		}
	}
	// 600 GB total, cap groups at 200 GB: the top directory cannot stay whole,
	// but each 120 GB season should. No two seasons fit in one 200 GB run
	// either, so sibling merging leaves this case exactly as it was.
	items := Group(files, 200*gb, Layout{})

	if len(items) != 5 {
		t.Fatalf("expected one item per season, got %d: %v", len(items), names(items))
	}
	for _, it := range items {
		if it.Size != 120*gb {
			t.Errorf("%s is %s, want 120 GB", it.Name, FormatBytes(it.Size))
		}
		if it.Size > 200*gb {
			t.Errorf("%s exceeds the group cap", it.Name)
		}
	}
	if items[0].Name != "/storage/Big/S00/" {
		t.Errorf("a run of one keeps the plain subtree name, got %q", items[0].Name)
	}
}

func TestGroupChunksLooseFiles(t *testing.T) {
	// 30 files of 10 GB sitting directly in one directory, capped at 100 GB.
	var files []*scan.File
	for i := range 30 {
		files = append(files, mkfile(fmt.Sprintf("/storage/Loose/f%02d.bin", i), 10*gb))
	}
	items := Group(files, 100*gb, Layout{})

	if len(items) != 3 {
		t.Fatalf("expected 3 chunks of 100 GB, got %d: %v", len(items), names(items))
	}
	var total int64
	for _, it := range items {
		if it.Size > 100*gb {
			t.Errorf("chunk %s is %s, over the cap", it.Name, FormatBytes(it.Size))
		}
		total += it.Size
	}
	if total != 300*gb {
		t.Errorf("chunks total %s, want 300 GB", FormatBytes(total))
	}
	// Chunks are cut in name order, so neighbours stay together.
	if !strings.Contains(items[0].Name, "f00.bin") {
		t.Errorf("first chunk should start at f00, got %q", items[0].Name)
	}
}

// TestGroupMergesFanoutSiblings is the gap DESIGN §4.1 records and the reason
// the sibling merge is not optional. A hash-fanout tree — immich's thumbnail
// store is the real one — has thousands of tiny leaf directories under a parent
// far larger than the cap. Descending into each of them one at a time produced
// 107,051 items over the real archive, 99,492 of them under 10 MB.
func TestGroupMergesFanoutSiblings(t *testing.T) {
	var files []*scan.File
	for i := range 2000 {
		files = append(files, mkfile(fmt.Sprintf("/storage/thumbs/%02x/%02x/img.jpg", i/256, i%256), 1*mb))
	}
	// 8 first-level directories of 256 MB each, every one holding 256 leaf
	// directories of a single 1 MB file. Under a 200 MB cap each first-level
	// directory has to break up, and its leaves then merge into 200 MB + 56 MB.
	items := Group(files, 200*mb, Layout{})

	if len(items) != 16 {
		t.Errorf("expected 8 directories × 2 runs = 16 items, got %d: %v", len(items), names(items))
	}
	// Without the merge this was one item per leaf directory.
	if len(items) >= 100 {
		t.Errorf("siblings were not merged: %d items for 2000 files", len(items))
	}
	for _, it := range items {
		if it.Size > 200*mb {
			t.Errorf("%s is %s, over the cap", it.Name, FormatBytes(it.Size))
		}
	}
	// Runs merge siblings, never cousins: a run must stay inside one parent, or
	// a restore of one directory would have to read items belonging to others.
	for _, it := range items {
		if !strings.HasPrefix(it.Name, "/storage/thumbs/") {
			t.Errorf("item %q escaped the fanout parent", it.Name)
		}
		for _, f := range it.Files {
			if !strings.HasPrefix(f.Path, it.Name[:len("/storage/thumbs/00")]) {
				t.Errorf("item %q holds %q from another parent", it.Name, f.Path)
			}
		}
	}
}

// TestGroupMergedRunNamesTheRangeItCovers keeps the plan readable by eye: a
// merged item has to say which siblings it holds, and must not claim a range it
// does not cover.
func TestGroupMergedRunNamesTheRangeItCovers(t *testing.T) {
	var files []*scan.File
	for _, dir := range []string{"aa", "bb", "cc"} {
		files = append(files, mkfile("/storage/fan/"+dir+"/x.bin", 30*mb))
	}
	// A fourth sibling too large to merge, sitting between the others in name
	// order. It must break the run rather than be swallowed by its name.
	files = append(files, mkfile("/storage/fan/bbb/huge.bin", 150*mb))

	items := Group(files, 100*mb, Layout{})

	var merged, standalone []string
	for _, it := range items {
		if strings.Contains(it.Name, "..") {
			merged = append(merged, it.Name)
		} else {
			standalone = append(standalone, it.Name)
		}
	}
	if len(merged) != 1 || merged[0] != "/storage/fan/[aa .. bb]/" {
		t.Errorf("expected one merged run over aa and bb, got %v (all: %v)", merged, names(items))
	}
	// bbb sorts between bb and cc, so a run spanning them would be named for a
	// range it does not hold. It has to break the run instead.
	if !slices.Contains(standalone, "/storage/fan/bbb/huge.bin") {
		t.Errorf("the oversized sibling should stand alone, got %v", standalone)
	}
	if !slices.Contains(standalone, "/storage/fan/cc/") {
		t.Errorf("cc should resume a fresh run after the break, got %v", standalone)
	}
	for _, it := range items {
		// A single file bigger than the cap has nowhere else to go; the packer
		// deals with it. Anything else over the cap is a grouping bug.
		if it.Size > 100*mb && len(it.Files) > 1 {
			t.Errorf("%s is %s, over the cap", it.Name, FormatBytes(it.Size))
		}
	}
	// Every file still lands somewhere exactly once.
	seen := make(map[string]bool)
	for _, it := range items {
		for _, f := range it.Files {
			if seen[f.Path] {
				t.Errorf("%s appears in more than one item", f.Path)
			}
			seen[f.Path] = true
		}
	}
	if len(seen) != len(files) {
		t.Errorf("grouped %d files, had %d", len(seen), len(files))
	}
}

func TestGroupPreservesEveryFile(t *testing.T) {
	var files []*scan.File
	for i := range 500 {
		files = append(files, mkfile(fmt.Sprintf("/storage/a%d/b%d/c%d/f%d", i%5, i%11, i%23, i), int64(i+1)*gb))
	}
	items := Group(files, 200*gb, Layout{})

	seen := make(map[string]bool)
	var total int64
	for _, it := range items {
		for _, f := range it.Files {
			if seen[f.Path] {
				t.Errorf("%s appears in more than one item", f.Path)
			}
			seen[f.Path] = true
			total += f.Size
		}
		if it.Count != len(it.Files) {
			t.Errorf("item %s: Count=%d but holds %d files", it.Name, it.Count, len(it.Files))
		}
		if it.Size > 200*gb && len(it.Files) > 1 {
			t.Errorf("item %s is %s, over the cap without being a single oversized file", it.Name, FormatBytes(it.Size))
		}
	}
	if len(seen) != len(files) {
		t.Errorf("grouped %d files, scanned %d", len(seen), len(files))
	}
	var want int64
	for _, f := range files {
		want += f.Size
	}
	if total != want {
		t.Errorf("grouped %s, scanned %s", FormatBytes(total), FormatBytes(want))
	}
}

func TestGroupEmptyInput(t *testing.T) {
	if items := Group(nil, 200*gb, Layout{}); items != nil {
		t.Errorf("expected no items for an empty file list, got %d", len(items))
	}
}

func TestFormatBytes(t *testing.T) {
	cases := map[int64]string{
		0:                 "0 B",
		999:               "999 B",
		2_500_000_000_000: "2.50 TB",
		1_500_000_000:     "1.50 GB",
	}
	for in, want := range cases {
		if got := FormatBytes(in); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
