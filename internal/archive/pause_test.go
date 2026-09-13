package archive

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kura/internal/catalog"
	"kura/internal/plan"
	"kura/internal/scan"
	"kura/internal/volume"
	"kura/internal/workdir"
)

// pauseAfter asks for a stop once the writer has been past n entries, which is
// what makes "mid-volume" mean something in a test: some objects landed, some
// did not, and the medium has to be legible either way.
func pauseAfter(n int) func() bool {
	seen := 0
	return func() bool {
		seen++
		return seen > n
	}
}

// pauseAfterFirstContainer asks for a stop as soon as a container has landed.
//
// Counting entries cannot express that: containers are built in one pass after
// every single object, so where the boundary falls depends on the plan's shape
// rather than on anything the test controls.
func pauseAfterFirstContainer() (func() bool, func(string, int64)) {
	placed := false
	return func() bool { return placed },
		func(name string, _ int64) {
			if strings.HasPrefix(name, containerPrefix) {
				placed = true
			}
		}
}

// planGrouped plans with a grouping bound of the caller's choosing.
//
// The fixture's own helper merges the whole tree into a single item, which makes
// two things unreachable: a second volume, because an item bigger than a volume
// is refused rather than split unless splitting is asked for, and a second
// container, because a container is an item. Tests that need either have to say
// where the items divide.
func planGrouped(t *testing.T, f *fixture, maxGroup int64) *plan.Plan {
	t.Helper()
	files, stats, err := scan.Walk([]scan.Root{{Scan: f.src}}, scan.NewExcluder(nil, true), func(string, error) {})
	if err != nil {
		t.Fatal(err)
	}
	d := scan.Diff(files, f.cat, scan.Options{Match: scan.MatchSample, Jobs: 2}, func(string, error) {})
	gen := f.cat.MaxGen() + 1
	lay := f.lay
	lay.Generation = gen
	items := plan.Group(d.ToWrite(), maxGroup, lay)
	p := plan.Pack(items, nil, plan.PackOptions{
		Capacity: f.cap, MaxBlanks: -1, NamePrefix: "V",
		Layout: lay, MetadataReserve: 64 << 10,
	})
	p.Generation = gen
	p.Finalize(d, stats, items)
	return p
}

// planTwoVolumes is planGrouped where the point is that the cycle spans
// cartridges.
func planTwoVolumes(t *testing.T, f *fixture, maxGroup int64) *plan.Plan {
	t.Helper()
	p := planGrouped(t, f, maxGroup)
	if len(p.Volumes) < 2 {
		t.Fatalf("this test needs two volumes; the plan has %d (warnings: %v)", len(p.Volumes), p.Warnings)
	}
	return p
}

// blindVolume fails every attempt to read a data object back.
//
// It is how a test proves a resume did not re-read the medium. The distinction
// is invisible in a temp directory and expensive on real media — re-reading a
// volume interrupted at 90% is hours before a new byte goes down — so the only
// way to hold the property is to make the read itself an error. Metadata objects
// are let through: Reconcile has to be able to ask the cartridge who it is.
type blindVolume struct {
	volume.Volume
	reads int
}

func (b *blindVolume) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	if strings.HasPrefix(name, dataPrefix) || strings.HasPrefix(name, containerPrefix) {
		b.reads++
		return nil, fmt.Errorf("%s was read back, but a pause had already recorded it", name)
	}
	return b.Volume.Get(ctx, name)
}

// unflushable is a drive whose Sync fails, which is the one failure a pause must
// not survive.
type unflushable struct{ volume.Volume }

func (unflushable) Sync(ctx context.Context) error { return errors.New("the drive did not flush") }

// pausedWrite runs one volume with a stop requested after n entries and hands
// back what the writer would have had the caller persist.
func pausedWrite(t *testing.T, f *fixture, pv *plan.Volume, gen, n int, tweak func(*Options)) *Result {
	t.Helper()
	vol, done := f.mount(pv.ID)
	defer done()
	opt := Options{Archive: f.cat.Header.Archive, Generation: gen, Pause: pauseAfter(n)}
	if tweak != nil {
		tweak(&opt)
	}
	res, err := Write(context.Background(), vol, pv, f.cat, opt)
	if err != nil {
		t.Fatalf("paused write of %s: %v", pv.ID, err)
	}
	if !res.Paused {
		t.Fatalf("the write of %s ran to the end; this test needs it to stop part way", pv.ID)
	}
	return res
}

// TestAPauseCommitsNothingToEitherCatalog is the load-bearing half of the whole
// design.
//
// A pause must leave the catalog alone in both places it lives. Locally,
// because alreadyWritten decides which volume the next run picks by looking for
// records of this generation on it, so committing them would make the resume
// skip the very cartridge it is meant to be finishing. And on the medium,
// because writeCatalog skips a name already present — so a mid-volume copy
// would be the one the cartridge keeps, leaving it indexing only what preceded
// the pause, which is the generation-numbering bug of DESIGN §8.1a arriving by
// another road.
func TestAPauseCommitsNothingToEitherCatalog(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{TarBelow: 1})
	f.write(map[string]int{"a.bin": 300 << 10, "b.bin": 300 << 10, "c.bin": 300 << 10, "d.bin": 300 << 10})
	p := f.plan()
	pv := p.Volumes[0]

	res := pausedWrite(t, f, pv, p.Generation, 2, nil)

	if len(f.cat.Records) != 0 {
		t.Errorf("a paused write put %d records in the catalog; the next run would skip this cartridge",
			len(f.cat.Records))
	}
	if alreadyOnVolume(f.cat, pv.ID, p.Generation) {
		t.Errorf("%s reads as written at generation %d after a pause", pv.ID, p.Generation)
	}

	vol, done := f.mount(pv.ID)
	defer done()
	objs, err := vol.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if isGenerationObject(o.Name) && strings.Contains(o.Name, "catalog") {
			t.Errorf("a paused write left %s on the medium; the volume now indexes only half a cycle", o.Name)
		}
	}
	if res.Checkpoint == nil {
		t.Fatal("a pause produced no checkpoint, so nothing tells the next run this was deliberate")
	}
	if len(res.Checkpoint.Objects) == 0 {
		t.Error("the checkpoint vouches for nothing, so the resume has to read the whole volume back")
	}
	if res.Remaining <= 0 {
		t.Errorf("the pause says %d entries are left; it stopped part way, so some must be", res.Remaining)
	}
}

// TestAPauseThatCouldNotFlushLeavesNoCheckpoint is the invariant the checkpoint
// rests on, tested by removing it.
//
// The checkpoint's whole claim is that what it describes is durable, which is
// why Sync runs before it is built and not after. If the flush fails there is
// nothing to vouch for, and a checkpoint written anyway would promise a host
// that it could be switched off over bytes still in the page cache.
func TestAPauseThatCouldNotFlushLeavesNoCheckpoint(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{TarBelow: 1})
	f.write(map[string]int{"a.bin": 300 << 10, "b.bin": 300 << 10, "c.bin": 300 << 10})
	p := f.plan()
	pv := p.Volumes[0]

	vol, done := f.mount(pv.ID)
	defer done()
	res, err := Write(context.Background(), unflushable{vol}, pv, f.cat, Options{
		Archive: f.cat.Header.Archive, Generation: p.Generation, Pause: pauseAfter(1),
	})
	if err == nil {
		t.Fatalf("a pause whose flush failed returned a result (paused=%v, checkpoint=%v) instead of an error",
			res.Paused, res.Checkpoint != nil)
	}
	if res != nil {
		t.Errorf("a failed flush still produced a result to persist: %+v", res)
	}
}

// TestAResumedPauseDoesNotReadTheMediumAgain is the reason a checkpoint exists
// at all.
//
// A crash leaves no records, so its resume has to reconstruct them by reading
// every object already on the volume. A pause saved them, so it does not — and
// the saving is not a convenience: on a volume interrupted near the end it is
// the difference between a resume that starts immediately and one that reads
// terabytes first. The blind volume turns that property into a test by making
// any read back an error.
func TestAResumedPauseDoesNotReadTheMediumAgain(t *testing.T) {
	f := newFixture(t, 8<<20, plan.Layout{TarBelow: 1 << 16})
	f.write(map[string]int{
		"big/one.bin": 400 << 10, "big/two.bin": 400 << 10,
		"small/a.txt": 900, "small/b.txt": 900, "small/c.txt": 900,
	})
	p := f.plan()
	pv := p.Volumes[0]

	first := pausedWrite(t, f, pv, p.Generation, 2, nil)
	vouched := len(first.Checkpoint.Objects)
	if vouched == 0 {
		t.Fatal("the pause vouched for nothing; there is no trust to test")
	}

	vol, done := f.mount(pv.ID)
	defer done()
	blind := &blindVolume{Volume: vol}
	res, err := Write(context.Background(), blind, pv, f.cat, Options{
		Archive: f.cat.Header.Archive, Generation: p.Generation, Resume: first.Checkpoint,
	})
	if err != nil {
		t.Fatalf("resuming a pause read the medium back: %v", err)
	}
	if blind.reads != 0 {
		t.Errorf("the resume read %d objects back; a pause had already recorded them", blind.reads)
	}
	if res.Adopted != vouched {
		t.Errorf("adopted %d of the %d objects the pause vouched for", res.Adopted, vouched)
	}
	if res.Resumed != 0 {
		t.Errorf("%d objects went the crash path even though the pause vouched for them", res.Resumed)
	}
	if res.Paused {
		t.Error("the resumed write reported itself paused with nothing asking it to stop")
	}

	// The finished volume has to be a volume, not just a resumed one.
	if got := len(res.Records); got != 5 {
		t.Errorf("the finished volume covers %d files, want 5", got)
	}
	vres, err := Verify(context.Background(), vol, f.cat, false)
	if err != nil {
		t.Fatal(err)
	}
	if vres.Failed() {
		t.Errorf("a volume finished from a pause does not verify: %v", vres.Findings)
	}
}

// TestASecondPauseHandsOnWhatTheFirstVouchedFor keeps a long write's trust from
// decaying one stop at a time.
//
// An operator pausing nightly would otherwise lose the first pause's objects to
// the second pause's checkpoint and be back to re-reading them, which defeats
// the point on exactly the volume that needed it most.
func TestASecondPauseHandsOnWhatTheFirstVouchedFor(t *testing.T) {
	f := newFixture(t, 8<<20, plan.Layout{TarBelow: 1})
	f.write(map[string]int{
		"a.bin": 200 << 10, "b.bin": 200 << 10, "c.bin": 200 << 10,
		"d.bin": 200 << 10, "e.bin": 200 << 10, "g.bin": 200 << 10,
	})
	p := f.plan()
	pv := p.Volumes[0]

	first := pausedWrite(t, f, pv, p.Generation, 2, nil)
	second := pausedWrite(t, f, pv, p.Generation, 4, func(o *Options) { o.Resume = first.Checkpoint })

	if len(second.Checkpoint.Objects) <= len(first.Checkpoint.Objects) {
		t.Fatalf("the second pause vouches for %d objects, the first for %d: the inheritance was dropped",
			len(second.Checkpoint.Objects), len(first.Checkpoint.Objects))
	}
	for _, was := range first.Checkpoint.Objects {
		if !vouchesFor(second.Checkpoint, was.Object) {
			t.Errorf("%s was vouched for by the first pause and is missing from the second", was.Object)
		}
	}

	// And the third run, blind, has to be able to finish on that inheritance
	// alone.
	vol, done := f.mount(pv.ID)
	defer done()
	blind := &blindVolume{Volume: vol}
	res, err := Write(context.Background(), blind, pv, f.cat, Options{
		Archive: f.cat.Header.Archive, Generation: p.Generation, Resume: second.Checkpoint,
	})
	if err != nil {
		t.Fatalf("finishing after two pauses: %v", err)
	}
	if blind.reads != 0 {
		t.Errorf("%d objects were read back after two pauses had recorded them", blind.reads)
	}
	if len(res.Records) != 6 {
		t.Errorf("the finished volume covers %d files, want 6", len(res.Records))
	}
}

// TestAPauseIsNotTrustedForAnotherVolume keeps a checkpoint bound to its cycle.
//
// A plan crossed with a catalog used to be unrecoverable (DESIGN §10.2), and a
// checkpoint is a third thing that can be crossed with them. It also has to
// survive the ordinary case of an operator who pauses volume 1 and chooses to
// write volume 2 next: that pause is not stale, it is about another cartridge,
// and consuming it would silently attach one volume's records to another.
func TestAPauseIsNotTrustedForAnotherVolume(t *testing.T) {
	f := newFixture(t, 700<<10, plan.Layout{TarBelow: 1})
	f.write(map[string]int{
		"one/a.bin": 200 << 10, "one/b.bin": 200 << 10,
		"two/c.bin": 200 << 10, "two/d.bin": 200 << 10,
	})
	p := planTwoVolumes(t, f, 500<<10)
	first := pausedWrite(t, f, p.Volumes[0], p.Generation, 1, nil)

	vol, done := f.mount(p.Volumes[1].ID)
	defer done()
	res, err := Write(context.Background(), vol, p.Volumes[1], f.cat, Options{
		Archive: f.cat.Header.Archive, Generation: p.Generation, Resume: first.Checkpoint,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Adopted != 0 {
		t.Errorf("%d objects were adopted from a pause belonging to %s while writing %s",
			res.Adopted, first.Checkpoint.Volume, res.Volume)
	}
	if !mentions(res.ResumeNotes, "leaving it alone") {
		t.Errorf("the other volume's pause was passed over in silence; notes were %q", res.ResumeNotes)
	}
}

// TestAPauseIsNotTrustedAfterTheSourceChanged is the check a re-read cannot do.
//
// Re-reading the medium compares it against itself, so a source rewritten at
// the same size with its mtime put back reads as protected with the old bytes'
// digest — the first hazard resume-trust.nix names. A checkpoint holds the
// source's ctime and inode as of the read, so the resume catches it for the
// price of a stat.
func TestAPauseIsNotTrustedAfterTheSourceChanged(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{TarBelow: 1})
	f.write(map[string]int{"a.bin": 300 << 10, "b.bin": 300 << 10, "c.bin": 300 << 10})
	p := f.plan()
	pv := p.Volumes[0]

	first := pausedWrite(t, f, pv, p.Generation, 2, nil)
	edited := first.Checkpoint.Objects[0].Sources[0].Path
	rewriteInPlace(t, edited, true) // mtime put back: ctime is the only trace

	vol, done := f.mount(pv.ID)
	defer done()
	res, err := Write(context.Background(), vol, pv, f.cat, Options{
		Archive: f.cat.Header.Archive, Generation: p.Generation, Resume: first.Checkpoint,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Adopted >= len(first.Checkpoint.Objects) {
		t.Errorf("adopted %d of %d objects; the edited source should have cost one of them",
			res.Adopted, len(first.Checkpoint.Objects))
	}
	if !mentions(res.ResumeNotes, filepath.Base(edited)) {
		t.Errorf("the edit to %s was not reported; notes were %q", edited, res.ResumeNotes)
	}
	// And the refusal has to be an outcome rather than a remark. The object's
	// name is taken on this volume and Put will not overwrite it, so the only
	// honest move is to report the file and record nothing — the next cycle puts
	// it on another cartridge. Falling back to re-reading the medium would
	// record the pre-pause bytes under the post-pause stat data, which is the
	// hazard resume-trust.nix exists for.
	if !reportedAsProblem(res, edited) {
		t.Errorf("%s was refused but not reported; the next cycle is never told to write it: %+v",
			edited, res.Problems)
	}
	if recordFor(res, edited) != nil {
		t.Errorf("%s was recorded from an object the resume refused to vouch for", edited)
	}
}

// TestAPauseIsNotTrustedWhenTheMediumDisagreesOnLength covers the other
// direction: the checkpoint describes the medium, and if the two do not match
// the checkpoint is the one that has to give way.
func TestAPauseIsNotTrustedWhenTheMediumDisagreesOnLength(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{TarBelow: 1})
	f.write(map[string]int{"a.bin": 300 << 10, "b.bin": 300 << 10, "c.bin": 300 << 10})
	p := f.plan()
	pv := p.Volumes[0]

	first := pausedWrite(t, f, pv, p.Generation, 2, nil)
	// The checkpoint now claims a length the medium does not have. Truncating is
	// not something a real medium does to a renamed object, but it is the shape
	// of every disagreement between the two, and it must not be resolved in the
	// checkpoint's favour.
	victim := first.Checkpoint.Objects[0]
	first.Checkpoint.Objects[0].Stored = victim.Stored - 1

	vol, done := f.mount(pv.ID)
	defer done()
	res, err := Write(context.Background(), vol, pv, f.cat, Options{
		Archive: f.cat.Header.Archive, Generation: p.Generation, Resume: first.Checkpoint,
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := countMentions(res.ResumeNotes, "the pause recorded"); n != 1 {
		t.Errorf("a length the medium disagrees with produced %d notes, want exactly 1: %q",
			n, res.ResumeNotes)
	}
	if len(res.Problems) == 0 {
		t.Error("an object the medium and the checkpoint disagree about was passed over without a report")
	}
	for _, rec := range res.Records {
		if rec.Object == victim.Object {
			t.Errorf("%s was recorded despite the disagreement about its length", victim.Object)
		}
	}
}

// TestVerifyResumedRefusesAnObjectThatDoesNotMatchThePause gives the flag
// something to prove.
//
// -verify-resumed exists for an operator who would rather pay for the read than
// trust the pause. With a zero-filled object — the shape a power cut leaves — it
// has to refuse the record rather than confirm it. What happens after the
// refusal is the crash path's business and is covered elsewhere.
func TestVerifyResumedRefusesAnObjectThatDoesNotMatchThePause(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{TarBelow: 1})
	f.write(map[string]int{"a.bin": 300 << 10, "b.bin": 300 << 10, "c.bin": 300 << 10})
	p := f.plan()
	pv := p.Volumes[0]

	first := pausedWrite(t, f, pv, p.Generation, 2, nil)
	damaged := first.Checkpoint.Objects[0].Object
	zeroFill(t, filepath.Join(f.shelf, pv.ID, filepath.FromSlash(damaged)))

	vol, done := f.mount(pv.ID)
	defer done()
	res, err := Write(context.Background(), vol, pv, f.cat, Options{
		Archive: f.cat.Header.Archive, Generation: p.Generation,
		Resume: first.Checkpoint, VerifyResumed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !mentions(res.ResumeNotes, "not what the pause recorded") {
		t.Errorf("a zero-filled object passed -verify-resumed; notes were %q", res.ResumeNotes)
	}
	if res.Adopted >= len(first.Checkpoint.Objects) {
		t.Errorf("adopted %d of %d objects despite one being damaged", res.Adopted, len(first.Checkpoint.Objects))
	}
}

// TestAPauseCarriesForwardWhatItCouldNotRead stops a stop from swallowing the
// one list the operator has to see.
//
// A source the writer could not read is reported and left out of the catalog, so
// the next cycle tries again. Those reports live in the Result, which a pause
// throws away — so without carrying them in the checkpoint, pausing a volume
// would quietly lose every such file from the final report.
func TestAPauseCarriesForwardWhatItCouldNotRead(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{TarBelow: 1})
	f.write(map[string]int{
		"aa-gone.bin": 300 << 10, "b.bin": 300 << 10, "c.bin": 300 << 10,
		"d.bin": 300 << 10, "e.bin": 300 << 10, "g.bin": 300 << 10,
	})
	p := f.plan()
	pv := p.Volumes[0]

	// Removed after planning, so the writer meets an entry whose file is gone.
	// Named to sort first, so a stop part way through has certainly met it.
	if err := os.Remove(filepath.Join(f.src, "aa-gone.bin")); err != nil {
		t.Fatal(err)
	}

	first := pausedWrite(t, f, pv, p.Generation, 4, nil)
	if len(first.Checkpoint.Problems) == 0 {
		t.Fatal("the pause recorded no problems; the missing file was lost from the report")
	}

	vol, done := f.mount(pv.ID)
	defer done()
	res, err := Write(context.Background(), vol, pv, f.cat, Options{
		Archive: f.cat.Header.Archive, Generation: p.Generation, Resume: first.Checkpoint,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reportedAsProblem(res, filepath.Join(f.src, "aa-gone.bin")) {
		t.Errorf("the file the pause could not read is absent from the finished volume's report: %+v", res.Problems)
	}
	for _, rec := range res.Records {
		if strings.HasSuffix(rec.Path, "aa-gone.bin") {
			t.Error("a file that was never written is recorded as stored")
		}
	}
}

// TestAPausedContainerIsAdoptedWholeOrNotAtAll covers the one object that holds
// more than one file.
//
// A container's membership is fixed when it is written. If the pause and the
// plan disagree about what is inside one, the slow path is the only thing
// entitled to decide, because Put will not overwrite the name and guessing is
// how data is lost.
func TestAPausedContainerIsAdoptedWholeOrNotAtAll(t *testing.T) {
	f := newFixture(t, 8<<20, plan.Layout{TarBelow: 1 << 16})
	f.write(map[string]int{
		"docs/a.txt": 800, "docs/b.txt": 800, "docs/c.txt": 800,
		"notes/d.txt": 800, "notes/e.txt": 800,
		"big.bin": 400 << 10,
	})
	// 2500 keeps docs/ and notes/ as separate items, so there are two
	// containers and therefore a boundary between them to stop at.
	p := planGrouped(t, f, 2500)
	pv := p.Volumes[0]

	stop, watch := pauseAfterFirstContainer()
	first := pausedWrite(t, f, pv, p.Generation, 0, func(o *Options) {
		o.Pause = stop
		o.OnObject = watch
	})
	var container *Placed
	for i := range first.Checkpoint.Objects {
		if strings.HasPrefix(first.Checkpoint.Objects[i].Object, containerPrefix) {
			container = &first.Checkpoint.Objects[i]
		}
	}
	if container == nil {
		t.Fatal("the pause placed no container; this test needs one")
	}
	if len(container.Records) != len(container.Sources) {
		t.Errorf("the container vouches for %d records with %d source stamps; every member needs one",
			len(container.Records), len(container.Sources))
	}

	// Drop one member's record, as if the pause had known less than the plan
	// does. The container must then be refused rather than adopted short.
	container.Records = container.Records[:len(container.Records)-1]

	vol, done := f.mount(pv.ID)
	defer done()
	res, err := Write(context.Background(), vol, pv, f.cat, Options{
		Archive: f.cat.Header.Archive, Generation: p.Generation, Resume: first.Checkpoint,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !mentions(res.ResumeNotes, "the plan puts in it") {
		t.Errorf("a container the pause knew less about was adopted anyway; notes were %q", res.ResumeNotes)
	}
}

// TestCheckpointRoundTripsThroughTheWorkdir covers the file itself: a missing
// one is the ordinary case and not an error, and a format this build does not
// recognise is refused rather than half understood.
func TestCheckpointRoundTripsThroughTheWorkdir(t *testing.T) {
	for _, name := range []string{workdir.ResumeName, "resume.jsonl"} {
		t.Run(name, func(t *testing.T) { checkpointRoundTrip(t, name) })
	}
}

// checkpointRoundTrip runs the round trip against one file name. Both are
// exercised because the suffix is what selects gzip: the real one is compressed,
// and the plain form is what anyone debugging by hand will reach for.
func checkpointRoundTrip(t *testing.T, name string) {
	dir := t.TempDir()
	path := filepath.Join(dir, name)

	got, err := LoadCheckpoint(path)
	if err != nil || got != nil {
		t.Fatalf("a workdir with no pause in it gave (%v, %v), want (nil, nil)", got, err)
	}

	want := &Checkpoint{
		Archive: "arch", Plan: "plan-1", Generation: 3, Volume: "V0002", Paused: "now",
		Objects: []Placed{{
			Object:  "data/a.bin",
			Stored:  7,
			Records: []*catalog.Record{{Path: "/src/a.bin", Size: 7}},
			Sources: []Source{{Path: "/src/a.bin", Size: 7, CTime: 12, Inode: 34}},
		}},
		Problems: []CheckpointProblem{{Path: "/src/gone", Err: "no such file"}},
	}
	if err := SaveCheckpoint(path, want); err != nil {
		t.Fatal(err)
	}
	got, err = LoadCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Format != ResumeFormat {
		t.Errorf("format came back %q, want %q", got.Format, ResumeFormat)
	}
	if got.Volume != want.Volume || got.Generation != want.Generation || got.Plan != want.Plan {
		t.Errorf("the cycle it belongs to did not survive the round trip: %+v", got)
	}
	if len(got.Objects) != 1 || got.Objects[0].Sources[0].CTime != 12 {
		t.Errorf("the source stamp did not survive, so the resume has nothing to check: %+v", got.Objects)
	}
	if len(got.Problems) != 1 || got.Problems[0].Path != "/src/gone" {
		t.Errorf("the problem list did not survive: %+v", got.Problems)
	}

	// A format this build does not know must be refused rather than half
	// understood. Written by hand, because SaveCheckpoint stamps the current
	// format and so cannot produce a foreign file.
	writeRaw(t, path, `{"_format":"kura-resume-99","volume":"V0002"}`+"\n")
	if _, err := LoadCheckpoint(path); err == nil {
		t.Error("a checkpoint from a build this one does not understand was loaded anyway")
	}

	if err := RemoveCheckpoint(path); err != nil {
		t.Fatal(err)
	}
	if err := RemoveCheckpoint(path); err != nil {
		t.Errorf("removing a checkpoint that is already gone failed: %v", err)
	}
}

// TestDescribesRefusesEveryCrossedPair enumerates the gates, because each one
// of them was a way to attach one cycle's records to another.
func TestDescribesRefusesEveryCrossedPair(t *testing.T) {
	cp := &Checkpoint{Archive: "A", Plan: "P", Generation: 2, Volume: "V1"}
	cases := []struct {
		name                    string
		archive, plan, volumeID string
		gen                     int
		want                    bool
	}{
		{"the pair it was made from", "A", "P", "V1", 2, true},
		{"another archive", "B", "P", "V1", 2, false},
		{"another plan, same generation", "A", "Q", "V1", 2, false},
		{"another generation", "A", "P", "V1", 3, false},
		{"another volume", "A", "P", "V2", 2, false},
		{"a plan with no ID to compare", "A", "", "V1", 2, true},
	}
	for _, c := range cases {
		if got := cp.Describes(c.archive, c.plan, c.gen, c.volumeID); got != c.want {
			t.Errorf("%s: Describes = %v, want %v", c.name, got, c.want)
		}
	}
	if (*Checkpoint)(nil).Describes("A", "P", 2, "V1") {
		t.Error("a workdir with no pause in it claimed to describe a volume")
	}
}

// alreadyOnVolume mirrors what cmd/kura uses to choose the next volume. A paused
// volume must not look written to it.
func alreadyOnVolume(cat *catalog.Catalog, id string, gen int) bool {
	for _, rec := range cat.OnVolume(id) {
		if rec.Gen == gen {
			return true
		}
	}
	return false
}

func vouchesFor(cp *Checkpoint, object string) bool {
	for _, pl := range cp.Objects {
		if pl.Object == object {
			return true
		}
	}
	return false
}

func mentions(notes []string, substr string) bool { return countMentions(notes, substr) > 0 }

// countMentions exists because a note said twice is a note the operator has to
// reconcile with itself. The refusal was reported both when the checkpoint was
// loaded and again when an entry met it.
func countMentions(notes []string, substr string) int {
	n := 0
	for _, note := range notes {
		if strings.Contains(note, substr) {
			n++
		}
	}
	return n
}

// TestAStoppedVerifyIsNotAVerdict keeps a half-read volume from looking checked.
//
// Three things would otherwise go wrong at once. A scrub that read nothing
// reports no findings, so it reads as Clean; it would stamp LastVerified and
// take the volume out of the overdue list for months; and every object it never
// reached is absent from the claimed set, so the unclaimed report would name most
// of the volume — an alarm raised by the operator's own Ctrl-C.
func TestAStoppedVerifyIsNotAVerdict(t *testing.T) {
	f := newFixture(t, 8<<20, plan.Layout{TarBelow: 1 << 16})
	f.write(map[string]int{
		"a.bin": 300 << 10, "b.bin": 300 << 10, "c.bin": 300 << 10,
		"docs/x.txt": 900, "docs/y.txt": 900,
	})
	p := f.plan()
	f.writeAll(p)

	vol, done := f.mount(p.Volumes[0].ID)
	defer done()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already stopped when Verify starts: the earliest possible boundary
	res, err := Verify(ctx, vol, f.cat, true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stopped {
		t.Fatal("a cancelled scrub ran to the end")
	}
	if len(res.Unclaimed) != 0 {
		t.Errorf("a scrub that stopped early called %d objects unaccounted for: %v",
			len(res.Unclaimed), res.Unclaimed)
	}
	if got := f.cat.Header.Volume(res.Volume).LastVerified; got != "" {
		t.Errorf("LastVerified was stamped as %q by a scrub that read %d objects", got, res.Checked)
	}

	// The same volume, read to the end, still has to come out clean — a stop
	// must leave nothing behind.
	full, err := Verify(context.Background(), vol, f.cat, true)
	if err != nil {
		t.Fatal(err)
	}
	if full.Stopped || full.Failed() {
		t.Errorf("the volume does not verify after a stopped scrub: stopped=%v findings=%v",
			full.Stopped, full.Findings)
	}
	if f.cat.Header.Volume(res.Volume).LastVerified == "" {
		t.Error("a complete scrub after a stopped one did not stamp LastVerified")
	}
}

// TestAStoppedRestoreIsNotOK keeps a half-produced tree from reading as a
// finished one.
//
// A restore needs no checkpoint — every file goes through a temporary and a
// rename, so what it wrote is whole — but the tree is not the tree, and OK() is
// what the caller turns into an exit status.
func TestAStoppedRestoreIsNotOK(t *testing.T) {
	f := newFixture(t, 8<<20, plan.Layout{TarBelow: 1 << 16})
	f.write(map[string]int{"a.bin": 300 << 10, "b.bin": 300 << 10, "docs/x.txt": 900})
	p := f.plan()
	f.writeAll(p)

	vol, done := f.mount(p.Volumes[0].ID)
	defer done()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dest := filepath.Join(t.TempDir(), "into")
	res, err := Restore(ctx, vol, f.cat, p.Generation, dest, true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stopped {
		t.Fatal("a cancelled restore ran to the end")
	}
	if res.OK() {
		t.Error("a restore that stopped part way reported itself complete")
	}
}

// TestAResumedVolumeReportsWhatTheCartridgeHolds keeps the inventory honest
// across a pause.
//
// The volume's object and byte counters were only ever incremented by the run
// that did the writing, so a cartridge finished from a pause that vouched for all
// but one of its objects was listed as holding that one object. On a real volume
// that is most of a terabyte of under-reporting, in the table an operator uses to
// decide what to buy and what to scrub. A crash resume had the same hole; a pause
// makes it the ordinary case rather than the exception.
func TestAResumedVolumeReportsWhatTheCartridgeHolds(t *testing.T) {
	f := newFixture(t, 8<<20, plan.Layout{TarBelow: 1})
	f.write(map[string]int{
		"a.bin": 200 << 10, "b.bin": 200 << 10, "c.bin": 200 << 10,
		"d.bin": 200 << 10, "e.bin": 200 << 10, "g.bin": 200 << 10,
	})
	p := f.plan()
	pv := p.Volumes[0]

	// What the volume would report if it had been written in one go.
	straight := newFixture(t, 8<<20, plan.Layout{TarBelow: 1})
	straight.src, straight.shelf = f.src, filepath.Join(t.TempDir(), "shelf")
	sp := straight.plan()
	straight.writeAll(sp)
	wantObjects := straight.cat.Header.Volume(sp.Volumes[0].ID).Objects
	wantBytes := straight.cat.Header.Volume(sp.Volumes[0].ID).Bytes

	first := pausedWrite(t, f, pv, p.Generation, 3, nil)
	vol, done := f.mount(pv.ID)
	defer done()
	res, err := Write(context.Background(), vol, pv, f.cat, Options{
		Archive: f.cat.Header.Archive, Generation: p.Generation, Resume: first.Checkpoint,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Adopted == 0 {
		t.Fatal("nothing was adopted, so this proves nothing about carried bytes")
	}

	got := f.cat.Header.Volume(res.Volume)
	if got.Objects != wantObjects {
		t.Errorf("the inventory lists %d objects on %s; writing the same volume in one go lists %d",
			got.Objects, res.Volume, wantObjects)
	}
	if got.Bytes != wantBytes {
		t.Errorf("the inventory lists %d bytes on %s; writing the same volume in one go lists %d",
			got.Bytes, res.Volume, wantBytes)
	}
	// And the throughput figure must not have been inflated to match: Bytes on
	// the Result is what this run actually moved.
	if res.Bytes >= wantBytes {
		t.Errorf("the resumed run claims to have written %d of the volume's %d bytes; "+
			"the adopted objects were counted into its throughput", res.Bytes, wantBytes)
	}
}

// TestAPausedFragmentIsAdoptedAndChecked covers the one object the writer only
// ever sees a piece of.
//
// A split file's cartridges are written days apart, which is exactly the window a
// pause widens, so the fragment path needs the same answers as the others: adopt
// what a pause vouched for, and refuse it when the medium no longer matches.
//
// The checkpoint here is assembled from a real write rather than produced by a
// real pause, because the packer gives a volume filled by one fragment exactly
// one entry — and the stop is checked between entries, so there is no boundary
// inside such a volume to pause at. The first half of the test is that degenerate
// case, which is worth pinning on its own.
func TestAPausedFragmentIsAdoptedAndChecked(t *testing.T) {
	f := newFixture(t, 1<<20, plan.Layout{TarBelow: -1})
	f.write(map[string]int{"huge.bin": (2 << 20) + 12345})
	p := f.plan(func(o *plan.PackOptions) {
		o.AllowSplit = true
		o.SplitMin = 1
	})
	if len(p.Volumes) < 3 {
		t.Fatalf("this test needs the file to span three volumes, it spans %d", len(p.Volumes))
	}
	pv := p.Volumes[0]

	// A stop asked for before the first entry has to place nothing and vouch for
	// nothing, rather than producing a checkpoint about an empty volume.
	vol, done := f.mount(pv.ID)
	res, err := Write(context.Background(), vol, pv, f.cat, Options{
		Archive: f.cat.Header.Archive, Generation: p.Generation, Pause: func() bool { return true },
	})
	done()
	if err != nil {
		t.Fatal(err)
	}
	if !res.Paused || res.Remaining != len(pv.Entries) {
		t.Fatalf("a stop before the first entry wrote something: paused=%v remaining=%d of %d",
			res.Paused, res.Remaining, len(pv.Entries))
	}
	if len(res.Checkpoint.Objects) != 0 {
		t.Errorf("a pause that placed nothing vouched for %d objects", len(res.Checkpoint.Objects))
	}
	if len(f.cat.Records) != 0 {
		t.Errorf("a pause that placed nothing still put %d records in the catalog", len(f.cat.Records))
	}

	// Now place the fragment for real, and build the checkpoint a pause would
	// have written if the volume had held anything after it.
	placed := placeWithoutCatalog(t, f, pv, p.Generation)
	rec := placed.Records[0]
	if len(rec.Parts) != 1 {
		t.Fatalf("the fragment's record carries %d parts, want 1", len(rec.Parts))
	}
	part := rec.Parts[0]
	info, err := os.Stat(filepath.Join(f.src, "huge.bin"))
	if err != nil {
		t.Fatal(err)
	}
	cp := &Checkpoint{
		Format: ResumeFormat, Archive: f.cat.Header.Archive,
		Generation: p.Generation, Volume: pv.ID,
		Objects: []Placed{{
			Object:  part.Object,
			Stored:  part.Length,
			Records: []*catalog.Record{rec},
			Sources: []Source{sourceOf(filepath.Join(f.src, "huge.bin"), info)},
		}},
	}

	vol, done = f.mount(pv.ID)
	defer done()
	res, err = Write(context.Background(), vol, pv, f.cat, Options{
		Archive: f.cat.Header.Archive, Generation: p.Generation,
		Resume: cp, VerifyResumed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Adopted != 1 {
		t.Errorf("the fragment was not adopted under -verify-resumed (adopted=%d, notes=%q)",
			res.Adopted, res.ResumeNotes)
	}
	if res.Resumed != 0 {
		t.Errorf("%d fragments were read back although the checkpoint vouched for them", res.Resumed)
	}

	// A fragment that did not survive has to be caught by the same flag. Its
	// digest is the only thing that can catch it: the length is right.
	zeroFill(t, filepath.Join(f.shelf, pv.ID, filepath.FromSlash(part.Object)))
	scratch := catalog.New()
	scratch.Header.Archive = f.cat.Header.Archive
	res, err = Write(context.Background(), vol, pv, scratch, Options{
		Archive: f.cat.Header.Archive, Generation: p.Generation,
		Resume: cp, VerifyResumed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Adopted != 0 {
		t.Error("a zero-filled fragment passed -verify-resumed")
	}
	if !mentions(res.ResumeNotes, "not what the pause recorded") {
		t.Errorf("the damaged fragment was not named; notes were %q", res.ResumeNotes)
	}
	if !reportedAsProblem(res, filepath.Join(f.src, "huge.bin")) {
		t.Errorf("the damaged fragment's file was not reported: %+v", res.Problems)
	}
}

// TestACrashResumedContainerWithAMissingSourceIsNotVouchedFor is about what a
// pause may inherit from a crash.
//
// A run can meet a container a *killed* run placed, read it back, and then be
// paused itself. It may only hand that container on if it could stat every
// member: one member whose source has gone means nothing can say whether the rest
// still matches the tree, and a checkpoint that vouched for it anyway would turn
// a crash's uncertainty into a pause's confidence.
func TestACrashResumedContainerWithAMissingSourceIsNotVouchedFor(t *testing.T) {
	f := newFixture(t, 8<<20, plan.Layout{TarBelow: 1 << 16})
	f.write(map[string]int{
		"docs/a.txt": 800, "docs/b.txt": 800, "docs/c.txt": 800,
		"notes/d.txt": 800, "notes/e.txt": 800,
	})
	p := planGrouped(t, f, 2500)
	pv := p.Volumes[0]

	// A killed run: the container lands, nothing is recorded anywhere.
	placeWithoutCatalog(t, f, pv, p.Generation)
	if err := os.Remove(filepath.Join(f.src, "docs", "b.txt")); err != nil {
		t.Fatal(err)
	}

	res := pausedWrite(t, f, pv, p.Generation, 6, nil)
	for _, pl := range res.Checkpoint.Objects {
		if strings.Contains(pl.Object, "docs") {
			t.Errorf("a container read back from a crash, one of whose sources has gone, was vouched for: %s",
				pl.Object)
		}
	}
	if !reportedAsProblem(res, filepath.Join(f.src, "docs", "b.txt")) {
		t.Errorf("the member whose source vanished was not reported: %+v", res.Problems)
	}
}

// TestAFileThatKeepsFailingIsNamedOnce keeps the report from growing a line per
// resume.
//
// An unreadable file is carried forward in the checkpoint so the operator is
// still told the next cycle owes it a write. The resume then meets its plan entry
// and fails on it again — so without an index by path, a volume paused three
// times would name the same file four times, and the count in the summary would
// be wrong with it.
func TestAFileThatKeepsFailingIsNamedOnce(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{TarBelow: 1})
	f.write(map[string]int{
		"aa-gone.bin": 300 << 10, "b.bin": 300 << 10, "c.bin": 300 << 10,
		"d.bin": 300 << 10, "e.bin": 300 << 10, "g.bin": 300 << 10,
	})
	p := f.plan()
	pv := p.Volumes[0]
	gone := filepath.Join(f.src, "aa-gone.bin")
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}

	first := pausedWrite(t, f, pv, p.Generation, 3, nil)
	second := pausedWrite(t, f, pv, p.Generation, 5, func(o *Options) { o.Resume = first.Checkpoint })

	vol, done := f.mount(pv.ID)
	defer done()
	res, err := Write(context.Background(), vol, pv, f.cat, Options{
		Archive: f.cat.Header.Archive, Generation: p.Generation, Resume: second.Checkpoint,
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, pr := range res.Problems {
		if pr.Path == gone {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("%s is named %d times after two pauses; the summary's count is wrong with it: %+v",
			gone, seen, res.Problems)
	}
}

// writeRaw writes a checkpoint file byte for byte, compressing it when the name
// says so, for the cases SaveCheckpoint will not produce.
func writeRaw(t *testing.T, path, body string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var w io.Writer = f
	if strings.HasSuffix(path, ".gz") {
		gz := gzip.NewWriter(f)
		defer gz.Close()
		w = gz
	}
	if _, err := io.WriteString(w, body); err != nil {
		t.Fatal(err)
	}
}

// TestATruncatedCheckpointIsRefusedRatherThanBelievedInPart covers the file
// losing its tail — a full disk during the pause is the likely way.
//
// The objects it still carries would each be gated individually, so adopting a
// prefix would be safe enough. Refusing is better: a file that lost its tail is
// one whose middle is also in doubt, and the caller's fallback is the crash path,
// which works. The count in the header is what makes the truncation visible at
// all.
func TestATruncatedCheckpointIsRefusedRatherThanBelievedInPart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, workdir.ResumeName)
	cp := &Checkpoint{
		Archive: "arch", Generation: 1, Volume: "V0001", Paused: "now",
		Objects: []Placed{
			{Object: "data/a.bin", Stored: 1, Records: []*catalog.Record{{Path: "/a.bin"}}},
			{Object: "data/b.bin", Stored: 2, Records: []*catalog.Record{{Path: "/b.bin"}}},
			{Object: "data/c.bin", Stored: 3, Records: []*catalog.Record{{Path: "/c.bin"}}},
		},
	}
	if err := SaveCheckpoint(path, cp); err != nil {
		t.Fatal(err)
	}
	if cp.Count != 3 {
		t.Fatalf("the header records %d objects, want 3", cp.Count)
	}

	// Re-written with the header intact and a line missing, which is the shape a
	// write that ran out of room leaves.
	short := &Checkpoint{Archive: "arch", Generation: 1, Volume: "V0001", Paused: "now", Objects: cp.Objects[:2]}
	if err := SaveCheckpoint(path, short); err != nil {
		t.Fatal(err)
	}
	short.Count = 3
	writeRawCheckpoint(t, path, short, cp.Objects[:2])

	if _, err := LoadCheckpoint(path); err == nil {
		t.Error("a truncated checkpoint was believed as far as it went")
	} else if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("the error does not say what is wrong: %v", err)
	}

	// The header alone still reads, which is what lets the report and the plan's
	// part-written test say something useful about a damaged pause.
	hdr, err := ReadCheckpointHeader(path)
	if err != nil {
		t.Fatalf("the header of a truncated checkpoint did not read: %v", err)
	}
	if hdr.Volume != "V0001" || hdr.Count != 3 {
		t.Errorf("the header came back as %+v", hdr)
	}
	if len(hdr.Objects) != 0 {
		t.Errorf("the header read decoded %d objects; it is meant to read one line", len(hdr.Objects))
	}
}

// writeRawCheckpoint writes a header that disagrees with the stream, which
// SaveCheckpoint will not do because it sets the count from the slice.
func writeRawCheckpoint(t *testing.T, path string, hdr *Checkpoint, objs []Placed) {
	t.Helper()
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	if err := enc.Encode(hdr); err != nil {
		t.Fatal(err)
	}
	for i := range objs {
		if err := enc.Encode(&objs[i]); err != nil {
			t.Fatal(err)
		}
	}
	writeRaw(t, path, sb.String())
}

// TestARefusedObjectIsRewrittenByTheNextCycle is the convergence half of the
// refusal, and the thing resume-trust.nix actually asks for.
//
// Refusing to record an object is only correct if something later puts the file
// back. Nothing recorded it as stored, so the next plan has to see it as a file
// still owed a write.
//
// It ought to land on a *different* cartridge, because an object's name is a pure
// function of its path and the name on this one is taken for good. That is not
// asserted here, and the reason is an open defect rather than a choice: `plan`
// allocates volume IDs from -start without consulting the inventory, so the next
// cycle reuses the ID it just wrote — the failure
// TestPlanNeverReusesAVolumeIDTheArchiveHasWritten in cmd/kura owns that. When it
// is fixed, the assertion to add here is that the entry's volume is not the one
// that refused it.
func TestARefusedObjectIsRewrittenByTheNextCycle(t *testing.T) {
	f := newFixture(t, 4<<20, plan.Layout{TarBelow: 1})
	f.write(map[string]int{"a.bin": 300 << 10, "b.bin": 300 << 10, "c.bin": 300 << 10})
	p := f.plan()
	pv := p.Volumes[0]

	first := pausedWrite(t, f, pv, p.Generation, 2, nil)
	edited := first.Checkpoint.Objects[0].Sources[0].Path
	rewriteInPlace(t, edited, true) // same size, mtime put back: ctime only

	vol, done := f.mount(pv.ID)
	res, err := Write(context.Background(), vol, pv, f.cat, Options{
		Archive: f.cat.Header.Archive, Generation: p.Generation, Resume: first.Checkpoint,
	})
	done()
	if err != nil {
		t.Fatal(err)
	}
	if recordFor(res, edited) != nil {
		t.Fatalf("%s was recorded, so nothing later will fix it", edited)
	}

	// The next cycle, planned against the catalog this one wrote.
	next := f.plan()
	found := false
	for _, nv := range next.Volumes {
		for i := range nv.Entries {
			if nv.Entries[i].Path == edited {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("%s is in no volume of the next cycle; the refusal lost the file", edited)
	}
}
