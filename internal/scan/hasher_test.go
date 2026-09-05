package scan

import (
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// TestHasherMatchesSampleHash is the property the writer depends on. It records
// the sampled digest from the stream it is already sending to the medium; the
// next cycle recomputes it with SampleHash from the file on disk. If the two
// ever disagreed, every file in the archive would look renamed.
func TestHasherMatchesSampleHash(t *testing.T) {
	dir := t.TempDir()
	rng := rand.New(rand.NewSource(5))

	sizes := []int{
		0, 1, 4096,
		sampleWindow - 1, sampleWindow, sampleWindow + 1,
		2*sampleWindow - 1, 2 * sampleWindow, 2*sampleWindow + 1,
		3 * sampleWindow,
	}
	for _, size := range sizes {
		buf := make([]byte, size)
		rng.Read(buf)
		path := filepath.Join(dir, "f")
		if err := os.WriteFile(path, buf, 0o644); err != nil {
			t.Fatal(err)
		}

		want, err := SampleHash(path, int64(size))
		if err != nil {
			t.Fatal(err)
		}
		wantFull, err := FullHash(path)
		if err != nil {
			t.Fatal(err)
		}

		// Feed it in awkward chunks: the writer's reader hands over whatever
		// the filesystem gives it, never a single tidy block.
		h := NewHasher(int64(size))
		for off := 0; off < size; {
			n := min(1+rng.Intn(3*sampleWindow/2), size-off)
			if _, err := h.Write(buf[off : off+n]); err != nil {
				t.Fatal(err)
			}
			off += n
		}
		sampled, full, err := h.Sums()
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if sampled != want {
			t.Errorf("size %d: streamed sample %s, SampleHash %s", size, sampled[:16], want[:16])
		}
		if full != wantFull {
			t.Errorf("size %d: streamed full digest disagrees with FullHash", size)
		}

		// Sums must be repeatable; a caller may ask twice.
		again, _, err := h.Sums()
		if err != nil || again != sampled {
			t.Errorf("size %d: Sums is not idempotent", size)
		}
	}
}

// TestHasherRejectsAShortStream guards the catalog against a digest computed
// over fewer bytes than the record claims. Both digests encode the length, so a
// truncated read would otherwise be recorded as a valid checksum of a lie.
func TestHasherRejectsAShortStream(t *testing.T) {
	h := NewHasher(100)
	h.Write(make([]byte, 60))
	if _, _, err := h.Sums(); err == nil {
		t.Error("a stream that stopped short must not produce a digest")
	}

	h = NewHasher(100)
	h.Write(make([]byte, 120))
	if _, _, err := h.Sums(); err == nil {
		t.Error("a stream that overran must not produce a digest")
	}
}

// TestHasherMemoryIsBounded keeps the trailing window a ring rather than a
// growing buffer: the archive holds 513 files over 10 GiB, and the writer runs
// several in parallel.
func TestHasherMemoryIsBounded(t *testing.T) {
	const size = 20 * sampleWindow
	h := NewHasher(size)
	chunk := make([]byte, sampleWindow/4)
	for range size / len(chunk) {
		h.Write(chunk)
	}
	if cap(h.tail) > sampleWindow {
		t.Errorf("tail buffer grew to %d bytes, want at most %d", cap(h.tail), sampleWindow)
	}
}
