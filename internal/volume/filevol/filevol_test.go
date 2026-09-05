package filevol

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kura/internal/volume"
)

func mount(t *testing.T, capacity int64) (*Backend, volume.Volume) {
	t.Helper()
	b := New(capacity)
	if err := b.Load(filepath.Join(t.TempDir(), "LTO001")); err != nil {
		t.Fatal(err)
	}
	v, err := b.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	return b, v
}

func put(t *testing.T, v volume.Volume, name, content string) {
	t.Helper()
	if err := v.Put(context.Background(), name, int64(len(content)), strings.NewReader(content)); err != nil {
		t.Fatalf("Put(%q): %v", name, err)
	}
}

func get(t *testing.T, v volume.Volume, name string) string {
	t.Helper()
	rc, err := v.Get(context.Background(), name)
	if err != nil {
		t.Fatalf("Get(%q): %v", name, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRoundTrip(t *testing.T) {
	_, v := mount(t, 1<<20)
	put(t, v, "hello.txt", "hello world")
	put(t, v, "nested/deep/file.bin", "payload")

	if got := get(t, v, "hello.txt"); got != "hello world" {
		t.Errorf("got %q", got)
	}
	if got := get(t, v, "nested/deep/file.bin"); got != "payload" {
		t.Errorf("got %q", got)
	}

	objs, err := v.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 {
		t.Fatalf("expected 2 objects, got %d: %+v", len(objs), objs)
	}
	// Names come back exactly as they went in, slashes included.
	if objs[0].Name != "hello.txt" || objs[1].Name != "nested/deep/file.bin" {
		t.Errorf("names round-tripped as %q, %q", objs[0].Name, objs[1].Name)
	}
	if objs[0].Size != 11 {
		t.Errorf("size = %d, want 11", objs[0].Size)
	}
}

// TestVolumeFull is one of the two reasons this backend exists: reaching the
// swap path in a test without writing terabytes.
func TestVolumeFull(t *testing.T) {
	_, v := mount(t, 100)
	put(t, v, "a", strings.Repeat("x", 60))

	_, free, err := v.Capacity()
	if err != nil {
		t.Fatal(err)
	}
	if free != 40 {
		t.Errorf("free = %d, want 40", free)
	}

	err = v.Put(context.Background(), "b", 60, strings.NewReader(strings.Repeat("y", 60)))
	if !errors.Is(err, volume.ErrVolumeFull) {
		t.Fatalf("expected ErrVolumeFull, got %v", err)
	}
	// A refused write leaves nothing behind.
	if _, err := v.Stat(context.Background(), "b"); !errors.Is(err, volume.ErrNotFound) {
		t.Errorf("refused object should not exist, got %v", err)
	}
	// And exactly filling it still works.
	put(t, v, "c", strings.Repeat("z", 40))
	if _, free, _ := v.Capacity(); free != 0 {
		t.Errorf("free = %d after exact fill, want 0", free)
	}
}

// TestPutIsAtomic covers the failure that would be silent and fatal: a write
// interrupted partway leaving a truncated object that a later run trusts.
func TestPutIsAtomic(t *testing.T) {
	_, v := mount(t, 1<<20)

	// A reader that dies halfway through.
	failing := io.MultiReader(
		strings.NewReader("first half"),
		badReader{errors.New("cable yanked")},
	)
	err := v.Put(context.Background(), "interrupted.bin", 100, failing)
	if err == nil {
		t.Fatal("expected the interrupted write to fail")
	}

	if _, err := v.Stat(context.Background(), "interrupted.bin"); !errors.Is(err, volume.ErrNotFound) {
		t.Errorf("a partial write must leave no object, got %v", err)
	}
	objs, _ := v.List(context.Background())
	if len(objs) != 0 {
		t.Errorf("volume should be empty, holds %+v", objs)
	}
	// The space is not charged either.
	if _, free, _ := v.Capacity(); free != 1<<20 {
		t.Errorf("free = %d, want the full %d back", free, 1<<20)
	}
}

// TestShortAndLongReaders guards against storing an object whose recorded
// length disagrees with its contents — a catalog that lies about sizes is worse
// than a write that failed.
func TestShortAndLongReaders(t *testing.T) {
	_, v := mount(t, 1<<20)

	if err := v.Put(context.Background(), "short", 100, strings.NewReader("only ten!!")); err == nil {
		t.Error("a reader shorter than the declared size must fail")
	}
	if err := v.Put(context.Background(), "long", 4, strings.NewReader("much longer than four")); err == nil {
		t.Error("a reader longer than the declared size must fail")
	}
	objs, _ := v.List(context.Background())
	if len(objs) != 0 {
		t.Errorf("neither should have been stored, got %+v", objs)
	}
}

// TestOverwriteRefused: on media that never reclaims space, writing a name
// twice means the catalog has lost track of what it already stored.
func TestOverwriteRefused(t *testing.T) {
	_, v := mount(t, 1<<20)
	put(t, v, "once.txt", "original")

	err := v.Put(context.Background(), "once.txt", 3, strings.NewReader("new"))
	if err == nil {
		t.Fatal("expected the second write to be refused")
	}
	if got := get(t, v, "once.txt"); got != "original" {
		t.Errorf("original content was disturbed: %q", got)
	}
}

// TestNamesCannotEscape: names arrive from a catalog that may have been read
// off a medium someone else wrote, so they are untrusted input.
func TestNamesCannotEscape(t *testing.T) {
	_, v := mount(t, 1<<20)
	for _, name := range []string{
		"../outside.txt",
		"nested/../../outside.txt",
		"/absolute.txt",
		"",
		".",
		"..",
		tmpDir,
		tmpDir + "/sneaky",
	} {
		if err := v.Put(context.Background(), name, 1, strings.NewReader("x")); err == nil {
			t.Errorf("Put(%q) should have been refused", name)
		}
		if _, err := v.Get(context.Background(), name); err == nil {
			t.Errorf("Get(%q) should have been refused", name)
		}
	}
}

// TestCorruptionIsVisible is the other reason this backend exists. Verify has
// to be able to detect bit rot, and bit rot cannot be arranged on demand
// against a tape.
func TestCorruptionIsVisible(t *testing.T) {
	b, v := mount(t, 1<<20)
	original := strings.Repeat("archive payload ", 64)
	put(t, v, "data/object.bin", original)

	// Reach behind the interface and flip a byte, exactly as decay would.
	onDisk := filepath.Join(b.dir, "data", "object.bin")
	raw, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)/2] ^= 0xFF
	if err := os.WriteFile(onDisk, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	if got := get(t, v, "data/object.bin"); got == original {
		t.Fatal("the corrupted byte did not survive to the reader")
	}
	// Size is unchanged, so a size-only check would miss it entirely — which is
	// why verification has to compare digests.
	obj, err := v.Stat(context.Background(), "data/object.bin")
	if err != nil {
		t.Fatal(err)
	}
	if obj.Size != int64(len(original)) {
		t.Errorf("size changed unexpectedly: %d", obj.Size)
	}
}

func TestEmptyDriveAndSwap(t *testing.T) {
	ctx := context.Background()
	b := New(1 << 20)

	// The backend names itself, and that is the only name it has: since a
	// volume's identity comes off the medium, "file" in an error message is
	// what says which kind of drive produced it.
	if b.Name() != "file" {
		t.Errorf("Name = %q, want file", b.Name())
	}

	if _, err := b.Open(ctx); !errors.Is(err, volume.ErrNoVolume) {
		t.Fatalf("empty drive should report ErrNoVolume, got %v", err)
	}
	// Releasing an empty drive is not an error.
	if err := b.Release(ctx); err != nil {
		t.Errorf("Release on an empty drive: %v", err)
	}

	root := t.TempDir()
	for _, id := range []string{"LTO001", "LTO002"} {
		if err := b.Load(filepath.Join(root, id)); err != nil {
			t.Fatalf("Load(%s): %v", id, err)
		}
		v, err := b.Open(ctx)
		if err != nil {
			t.Fatalf("Open(%s): %v", id, err)
		}
		// The marker stands in for identity, which the backend deliberately
		// does not carry: what distinguishes one loaded volume from another is
		// its contents, not the path it was mounted at.
		put(t, v, "marker", id)
		if err := b.Release(ctx); err != nil {
			t.Fatalf("Release(%s): %v", id, err)
		}
	}

	// Each volume kept its own contents across the swap.
	for _, id := range []string{"LTO001", "LTO002"} {
		if err := b.Load(filepath.Join(root, id)); err != nil {
			t.Fatal(err)
		}
		v, err := b.Open(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := get(t, v, "marker"); got != id {
			t.Errorf("%s holds %q", id, got)
		}
		b.Release(ctx)
	}
}

// TestReopenPreservesAccounting: a volume written across two sessions must not
// forget how full it is, or the second session will overfill it.
func TestReopenPreservesAccounting(t *testing.T) {
	ctx := context.Background()
	b := New(1000)
	dir := filepath.Join(t.TempDir(), "LTO009")

	if err := b.Load(dir); err != nil {
		t.Fatal(err)
	}
	v, err := b.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	put(t, v, "first", strings.Repeat("a", 600))
	if err := b.Release(ctx); err != nil {
		t.Fatal(err)
	}

	if err := b.Load(dir); err != nil {
		t.Fatal(err)
	}
	v, err = b.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Release(ctx)

	if _, free, _ := v.Capacity(); free != 400 {
		t.Errorf("free = %d after reopen, want 400", free)
	}
	if err := v.Put(ctx, "second", 500, strings.NewReader(strings.Repeat("b", 500))); !errors.Is(err, volume.ErrVolumeFull) {
		t.Errorf("expected the reopened volume to know it is nearly full, got %v", err)
	}
}

// TestTempDirIsInvisible: partial writes must never be reported as objects, nor
// charged against capacity, nor survive an unmount.
func TestTempDirIsInvisible(t *testing.T) {
	b, v := mount(t, 1<<20)
	put(t, v, "real.txt", "content")

	stray := filepath.Join(b.dir, tmpDir, "put-leftover")
	if err := os.WriteFile(stray, bytes.Repeat([]byte("x"), 500), 0o644); err != nil {
		t.Fatal(err)
	}

	objs, err := v.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].Name != "real.txt" {
		t.Errorf("temp files leaked into List: %+v", objs)
	}

	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stray); !errors.Is(err, os.ErrNotExist) {
		t.Error("partial writes should not survive an unmount")
	}
}

func TestClosedVolumeRefusesWork(t *testing.T) {
	_, v := mount(t, 1<<20)
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := v.Put(ctx, "x", 1, strings.NewReader("x")); !errors.Is(err, os.ErrClosed) {
		t.Errorf("Put after Close: %v", err)
	}
	if _, err := v.List(ctx); !errors.Is(err, os.ErrClosed) {
		t.Errorf("List after Close: %v", err)
	}
	if _, _, err := v.Capacity(); !errors.Is(err, os.ErrClosed) {
		t.Errorf("Capacity after Close: %v", err)
	}
	// Close is idempotent.
	if err := v.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestHealthIsInjectable(t *testing.T) {
	b, _ := mount(t, 1<<20)
	h, err := b.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != volume.StatusUnknown {
		t.Errorf("default status = %v, want unknown", h.Status)
	}

	b.SetHealth(volume.Health{
		Status:           volume.StatusDegrading,
		CleaningRequired: true,
		Detail:           map[string]string{"corrected_errors": "418"},
	})
	h, _ = b.Health(context.Background())
	if h.Status != volume.StatusDegrading || !h.CleaningRequired {
		t.Errorf("injected health did not come back: %+v", h)
	}
	if h.Detail["corrected_errors"] != "418" {
		t.Errorf("detail lost: %+v", h.Detail)
	}
}

func TestCapacityFromFilesystem(t *testing.T) {
	_, v := mount(t, 0) // 0 means ask the filesystem
	total, free, err := v.Capacity()
	if err != nil {
		t.Fatal(err)
	}
	if total <= 0 || free <= 0 {
		t.Errorf("filesystem capacity looks wrong: total=%d free=%d", total, free)
	}
}

func TestStatusString(t *testing.T) {
	for s, want := range map[volume.Status]string{
		volume.StatusUnknown:        "unknown",
		volume.StatusHealthy:        "healthy",
		volume.StatusDegrading:      "degrading",
		volume.StatusNeedsAttention: "needs attention",
	} {
		if got := s.String(); got != want {
			t.Errorf("Status(%d) = %q, want %q", s, got, want)
		}
	}
}

// TestManyObjects is a cheap stand-in for the scale the real archive reaches:
// 1.66 M objects on tape, so the backend must not be quadratic in object count.
func TestManyObjects(t *testing.T) {
	_, v := mount(t, 1<<24)
	const n = 2000
	for i := range n {
		put(t, v, fmt.Sprintf("shard%02d/obj%04d", i%16, i), fmt.Sprintf("payload-%d", i))
	}
	objs, err := v.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != n {
		t.Fatalf("stored %d objects, listed %d", n, len(objs))
	}
	for i := 1; i < len(objs); i++ {
		if objs[i-1].Name >= objs[i].Name {
			t.Fatalf("List is not name-ordered at %d: %q then %q", i, objs[i-1].Name, objs[i].Name)
		}
	}
}

// TestSyncMakesObjectsReadable checks the durability half of the contract that
// Put deliberately leaves alone.
func TestSyncMakesObjectsReadable(t *testing.T) {
	_, v := mount(t, 1<<20)
	put(t, v, "a/b/c.bin", "durable")
	put(t, v, "top.bin", "also durable")

	if err := v.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := get(t, v, "a/b/c.bin"); got != "durable" {
		t.Errorf("got %q", got)
	}

	// Sync must tolerate a volume holding nothing.
	_, empty := mount(t, 1<<20)
	if err := empty.Sync(context.Background()); err != nil {
		t.Errorf("Sync on an empty volume: %v", err)
	}
}

type badReader struct{ err error }

func (b badReader) Read([]byte) (int, error) { return 0, b.err }
