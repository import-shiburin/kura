package plan

import (
	"archive/tar"
	"bytes"
	"fmt"
	"strings"
	"testing"

	"kura/internal/scan"
)

// encodedSize builds the tar an item's container describes and reports how big
// it actually comes out, which is the number the estimate has to cover.
func encodedSize(t *testing.T, c Container) int64 {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range c.Files {
		if err := tw.WriteHeader(&tar.Header{
			Name: memberName(f.Path),
			Mode: 0o644,
			Size: f.Size,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(make([]byte, f.Size)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return int64(buf.Len())
}

// TestContainerEstimateCoversTheRealTar is the bug this accounting exists for.
// Packing by source bytes overfilled a volume by exactly the framing tar adds —
// 2.11 GB across the real archive's small files, and enough on any one volume
// to stop a write partway with the cartridge half full.
//
// The estimate must never come in under the truth. Coming in over costs a
// kilobyte of unused tape.
func TestContainerEstimateCoversTheRealTar(t *testing.T) {
	long := "/storage/" + strings.Repeat("deeply/nested/", 12) + "file"

	cases := []struct {
		name  string
		files []*scan.File
	}{
		{"short names", []*scan.File{
			mkfile("/a", 10), mkfile("/b", 511), mkfile("/c", 512), mkfile("/d", 513),
		}},
		{"empty files", []*scan.File{mkfile("/e1", 0), mkfile("/e2", 0)}},
		{"long names needing an extended header", []*scan.File{
			mkfile(long+"1.jpg", 4096), mkfile(long+"2.jpg", 4096),
		}},
		{"exactly at the ustar name limit", []*scan.File{
			mkfile("/"+strings.Repeat("x", 99), 100),
			mkfile("/"+strings.Repeat("y", 100), 100),
			mkfile("/"+strings.Repeat("z", 101), 100),
		}},
		{"one file", []*scan.File{mkfile("/only", 1)}},
	}

	lay := Layout{Generation: 1, TarBelow: 1 << 20}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			it := lay.makeItem("/item/", c.files, true)
			if len(it.Containers) != 1 {
				t.Fatalf("expected one container, got %d", len(it.Containers))
			}
			actual := encodedSize(t, it.Containers[0])
			if it.Containers[0].Size < actual {
				t.Errorf("estimated %d bytes, the tar is actually %d — a volume packed on this would overfill",
					it.Containers[0].Size, actual)
			}
			// Generous is fine, wildly generous is waste.
			if it.Containers[0].Size > actual+int64(len(c.files)+1)*1024 {
				t.Errorf("estimated %d bytes for a %d-byte tar, which over-reserves badly",
					it.Containers[0].Size, actual)
			}
		})
	}
}

// TestItemSizeIsStoredBytesNotSourceBytes keeps the packer working in the units
// the medium charges in. Two files of 10 bytes do not take 20 bytes on a volume
// once they are inside a container.
func TestItemSizeIsStoredBytesNotSourceBytes(t *testing.T) {
	files := []*scan.File{mkfile("/storage/a", 10), mkfile("/storage/b", 20)}

	loose := Layout{}.makeItem("/storage/", files, true)
	if loose.Size != 30 {
		t.Errorf("with no bundling an item is its source bytes, got %d", loose.Size)
	}
	if len(loose.Containers) != 0 || len(loose.Loose) != 2 {
		t.Errorf("nothing should be containerised, got %d containers", len(loose.Containers))
	}

	bundled := Layout{Generation: 1, TarBelow: 1 << 20}.makeItem("/storage/", files, true)
	if bundled.Size <= 30 {
		t.Errorf("a container costs more than its contents, got %d for 30 source bytes", bundled.Size)
	}
	if len(bundled.Containers) != 1 || len(bundled.Loose) != 0 {
		t.Errorf("both files should be in one container, got %d containers and %d loose",
			len(bundled.Containers), len(bundled.Loose))
	}
}

// TestLargeFilesStayOutOfContainers is the threshold doing its job: a file that
// carries real data keeps its own object, so damage costs it alone and a
// restore of one file does not read a container.
func TestLargeFilesStayOutOfContainers(t *testing.T) {
	lay := Layout{Generation: 3, TarBelow: 1 << 20}
	it := lay.makeItem("/storage/Movie/", []*scan.File{
		mkfile("/storage/Movie/film.mkv", 4<<30),
		mkfile("/storage/Movie/poster.jpg", 40<<10),
		mkfile("/storage/Movie/film.srt", 900),
	}, true)

	if len(it.Loose) != 1 || it.Loose[0].Path != "/storage/Movie/film.mkv" {
		t.Errorf("only the 4 GB file should stand alone, got %d loose", len(it.Loose))
	}
	if len(it.Containers) != 1 || len(it.Containers[0].Files) != 2 {
		t.Fatalf("the poster and subtitle should share one container, got %d", len(it.Containers))
	}
	if got := it.Containers[0].Name; got != "containers/g0003/storage/Movie.part001.tar" {
		t.Errorf("container name %q does not carry the generation and path", got)
	}
}

// TestContainersRollAtTheCap bounds both the staging file a container is built
// in and the blast radius of a torn one.
func TestContainersRollAtTheCap(t *testing.T) {
	var files []*scan.File
	for i := range 10 {
		files = append(files, mkfile(fmt.Sprintf("/storage/d/f%02d", i), 300))
	}
	lay := Layout{Generation: 1, TarBelow: 1 << 20, MaxContainer: 1000}
	it := lay.makeItem("/storage/d/", files, true)

	if len(it.Containers) != 4 {
		t.Fatalf("3000 source bytes at a 1000-byte cap should need 4 containers, got %d", len(it.Containers))
	}
	names := map[string]bool{}
	var members int
	for i, c := range it.Containers {
		if names[c.Name] {
			t.Errorf("container name %q reused", c.Name)
		}
		names[c.Name] = true
		if !strings.HasSuffix(c.Name, fmt.Sprintf("part%03d.tar", i+1)) {
			t.Errorf("container %d is named %q", i+1, c.Name)
		}
		members += len(c.Files)
	}
	if members != len(files) {
		t.Errorf("containers hold %d files, the item has %d", members, len(files))
	}
}

// TestEveryFileIsWrittenExactlyOnce is the conservation property the layout has
// to preserve: a file is either loose or in exactly one container, never both
// and never neither.
func TestEveryFileIsWrittenExactlyOnce(t *testing.T) {
	var files []*scan.File
	for i := range 200 {
		size := int64(i * 20000) // straddles the threshold
		files = append(files, mkfile(fmt.Sprintf("/storage/mix/f%03d", i), size))
	}
	it := Layout{Generation: 1, TarBelow: 1 << 20, MaxContainer: 1 << 16}.makeItem("/storage/mix/", files, true)

	seen := make(map[string]int)
	for _, f := range it.Loose {
		seen[f.Path]++
	}
	for _, c := range it.Containers {
		for _, f := range c.Files {
			seen[f.Path]++
		}
	}
	for _, f := range files {
		if seen[f.Path] != 1 {
			t.Errorf("%s is written %d times", f.Path, seen[f.Path])
		}
	}
	if len(seen) != len(files) {
		t.Errorf("laid out %d files, had %d", len(seen), len(files))
	}
}
