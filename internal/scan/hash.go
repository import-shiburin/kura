package scan

import (
	"bufio"
	"crypto/sha256"
	"encoding"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"sync"
)

// sampleWindow is how much is read from each end of a file for a sampled hash.
const sampleWindow = 1 << 20

// SampleHash digests a file's size plus its first and last megabyte.
//
// This exists only to answer "is this the same content under a new name?"
// cheaply. It reads at most 2 MiB regardless of file size, which is what makes
// rename detection affordable across a multi-terabyte tree. It is deliberately
// not an integrity checksum: the writer computes a full digest as it streams
// each file to media, where the bytes are being read anyway.
//
// The kind it produces is recorded as catalog.HashKindSample.
func SampleHash(path string, size int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	var sz [8]byte
	binary.LittleEndian.PutUint64(sz[:], uint64(size))
	h.Write(sz[:])

	head := min(int64(sampleWindow), size)
	if _, err := io.CopyN(h, f, head); err != nil && err != io.EOF {
		return "", err
	}
	// Only read a tail when it would not overlap the head.
	if size > 2*sampleWindow {
		if _, err := f.Seek(size-sampleWindow, io.SeekStart); err != nil {
			return "", err
		}
		if _, err := io.CopyN(h, f, sampleWindow); err != nil && err != io.EOF {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// FullHash digests a file end to end.
//
// Used to confirm a match that the sampled digest only shortlisted, and by the
// writer as it streams a file to media, where the bytes are being read anyway.
func FullHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, bufio.NewReaderSize(f, 1<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// HashAll fills in sampled hashes for the given files using a worker pool.
// Files that cannot be read are left with an empty hash rather than failing
// the run: an unreadable file simply does not participate in rename matching.
func HashAll(files []*File, jobs int, onErr func(string, error)) map[string]string {
	if len(files) == 0 {
		return nil
	}
	if jobs < 1 {
		jobs = 1
	}

	type result struct {
		path string
		hash string
	}

	in := make(chan *File)
	out := make(chan result)
	var wg sync.WaitGroup

	for range jobs {
		wg.Go(func() {
			for f := range in {
				h, err := SampleHash(f.OpenPath(), f.Size)
				if err != nil {
					onErr(f.OpenPath(), err)
					continue
				}
				// Keyed by canonical path: callers look results up by identity,
				// not by where the bytes happened to be read from.
				out <- result{f.Path, h}
			}
		})
	}
	go func() {
		for _, f := range files {
			in <- f
		}
		close(in)
		wg.Wait()
		close(out)
	}()

	hashes := make(map[string]string, len(files))
	for r := range out {
		hashes[r.path] = r.hash
	}
	return hashes
}

// SampleBytes is how much SampleHash actually reads for a file of this size,
// which is what the hashed-bytes accounting reports.
func SampleBytes(size int64) int64 { return min(size, 2*sampleWindow) }

// Hasher computes the sampled and whole-file digests of a stream in one pass.
//
// The writer is the one place both are wanted at once, and it already has every
// byte in flight on its way to the medium. Reading the file a second time to
// sample it would cost a seek and 2 MiB per object across the whole archive for
// a digest that can be derived from bytes already passing through.
//
// The sampled digest it produces is identical to SampleHash's, which is not a
// nicety: the next cycle compares this value against one computed that way, and
// a disagreement would make every file look renamed.
type Hasher struct {
	size  int64
	full  hash.Hash
	head  hash.Hash // size, then the leading window
	n     int64     // bytes seen
	tail  []byte    // ring holding the trailing window
	start int       // ring head, once it has wrapped
}

// NewHasher starts a hasher for a stream of exactly size bytes.
func NewHasher(size int64) *Hasher {
	h := &Hasher{size: size, full: sha256.New(), head: sha256.New()}
	var sz [8]byte
	binary.LittleEndian.PutUint64(sz[:], uint64(size))
	h.head.Write(sz[:])
	if size > 2*sampleWindow {
		h.tail = make([]byte, 0, sampleWindow)
	}
	return h
}

func (h *Hasher) Write(p []byte) (int, error) {
	h.full.Write(p)

	// The leading window, up to the point where SampleHash would stop reading.
	if remaining := min(int64(sampleWindow), h.size) - h.n; remaining > 0 {
		h.head.Write(p[:min(int64(len(p)), remaining)])
	}
	// The trailing window, kept in a ring so an arbitrarily long stream costs
	// one megabyte of memory rather than growing with the file.
	if h.tail != nil {
		h.keepTail(p)
	}
	h.n += int64(len(p))
	return len(p), nil
}

func (h *Hasher) keepTail(p []byte) {
	if len(p) >= sampleWindow {
		// This chunk alone covers the window; nothing older can survive.
		h.tail = append(h.tail[:0], p[len(p)-sampleWindow:]...)
		h.start = 0
		return
	}
	for _, b := range p {
		if len(h.tail) < sampleWindow {
			h.tail = append(h.tail, b)
			continue
		}
		h.tail[h.start] = b
		h.start = (h.start + 1) % sampleWindow
	}
}

// Sums returns the sampled and whole-file digests. It reports an error when the
// stream did not deliver exactly the declared size, because both digests encode
// that length and a mismatch would put a wrong value in the catalog.
func (h *Hasher) Sums() (sampled, full string, err error) {
	if h.n != h.size {
		return "", "", fmt.Errorf("hasher: declared %d bytes, saw %d", h.size, h.n)
	}
	sum := h.head
	if h.tail != nil {
		// Replay the ring in stream order, matching the seek-to-end read that
		// SampleHash performs.
		sum = cloneInto(h.head)
		sum.Write(h.tail[h.start:])
		sum.Write(h.tail[:h.start])
	}
	return hex.EncodeToString(sum.Sum(nil)), hex.EncodeToString(h.full.Sum(nil)), nil
}

// cloneInto exists because Sums may be called more than once and Sum must not
// see the tail replayed twice.
func cloneInto(h hash.Hash) hash.Hash {
	if c, ok := h.(encoding.BinaryMarshaler); ok {
		if b, err := c.MarshalBinary(); err == nil {
			n := sha256.New()
			if u, ok := n.(encoding.BinaryUnmarshaler); ok && u.UnmarshalBinary(b) == nil {
				return n
			}
		}
	}
	return h
}
