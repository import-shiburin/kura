package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"kura/internal/archive"
	"kura/internal/catalog"
	"kura/internal/plan"
	"kura/internal/workdir"
)

// TestAPauseRequestIsConsumedByTheRunThatHonoursIt is the difference between a
// request and a mode.
//
// The flag file has to be cleared by the write that acts on it. Left in place it
// would stop the resume on its own first object, and the operator would be stuck
// one object from where they started with no way out but to find and delete a
// file they were never told about.
func TestAPauseRequestIsConsumedByTheRunThatHonoursIt(t *testing.T) {
	dir := t.TempDir()
	flag := filepath.Join(dir, workdir.PauseName)
	w := &pauseWatcher{flag: flag}

	if w.Asked() {
		t.Fatal("a write with no request against it asked to stop")
	}
	if err := os.WriteFile(flag, []byte("requested\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The stat is throttled to once a second, because Asked is called between
	// every object and a real volume holds six figures of them. Forcing the look
	// is what a test does instead of sleeping.
	w.looked.Store(0)
	if !w.Asked() {
		t.Fatal("the request was not noticed")
	}
	if _, err := os.Stat(flag); !os.IsNotExist(err) {
		t.Errorf("the request outlived the run that honoured it (%v); the resume would stop at once", err)
	}
	if w.Reason() != "kura pause" {
		t.Errorf("the checkpoint would record the reason as %q", w.Reason())
	}
	// Still asked afterwards: the writer polls repeatedly and every poll from
	// here on has to agree, or it would place one more object each time round.
	if !w.Asked() {
		t.Error("the request was forgotten as soon as it was read")
	}
}

// TestAStalePauseRequestIsClearedRatherThanHonoured keeps yesterday's request
// from stopping today's write on its first object.
//
// A request is about the run that was in flight when it was made. A new
// invocation is the operator asking for work.
func TestAStalePauseRequestIsClearedRatherThanHonoured(t *testing.T) {
	dir := t.TempDir()
	wd := workdir.At(dir)
	if err := os.WriteFile(wd.Pause(), []byte("yesterday\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := watchPause(wd)
	if _, err := os.Stat(wd.Pause()); !os.IsNotExist(err) {
		t.Errorf("a stale request survived startup (%v)", err)
	}
	if w.Asked() {
		t.Error("a write stopped on its first object because of an earlier run's request")
	}
}

// TestAPauseNamesWhatItIsWaitingFor is why OnStart exists.
//
// The stop lands at the next object boundary, and for one of the 513 files in the
// real archive over 10 GiB that is minutes away. A run that acknowledged nothing
// would be indistinguishable from one that had hung.
func TestAPauseNamesWhatItIsWaitingFor(t *testing.T) {
	w := &pauseWatcher{}
	if got := w.inFlight(); got != "the object in flight" {
		t.Errorf("before anything is placed the wait is described as %q", got)
	}
	w.Starting("data/srv/big.bin", 11<<30)
	got := w.inFlight()
	if got != "data/srv/big.bin (11.81 GB)" {
		t.Errorf("the wait is described as %q, which does not say how long it will be", got)
	}
}

// TestAPausedVolumeCountsAsAPartWrittenCycle closes the trap that would have
// made a pause lose a cartridge.
//
// `plan` refuses to replace a plan whose cycle is unfinished, judged by whether
// any volume holds records of that generation. A pause deliberately writes no
// records — that is the whole design — so a pause on the *first* volume left
// that test at zero, and the next `kura plan` would have replaced the plan
// without complaint. The cartridge would then hold objects that nothing indexes
// and that no plan can finish, on media that never reclaims the space.
func TestAPausedVolumeCountsAsAPartWrittenCycle(t *testing.T) {
	mk := func(t *testing.T, withPause bool) (*plan.Plan, []string) {
		t.Helper()
		dir := t.TempDir()
		wd := workdir.At(dir)
		p := &plan.Plan{
			Format: plan.Format, Generation: 4, ID: "plan-4",
			Volumes: []*plan.Volume{{ID: "LTO001"}, {ID: "LTO002"}},
		}
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(wd.Plan(), b, 0o644); err != nil {
			t.Fatal(err)
		}
		if withPause {
			cp := &archive.Checkpoint{
				Archive: "arch", Plan: "plan-4", Generation: 4, Volume: "LTO001",
			}
			if err := archive.SaveCheckpoint(wd.Resume(), cp); err != nil {
				t.Fatal(err)
			}
		}
		// An empty catalog: nothing has been committed, which is exactly the
		// state a pause on the first volume leaves behind.
		cat := catalog.New()
		cat.Header.Archive = "arch"
		return unfinishedCycle(wd, cat)
	}

	if prev, left := mk(t, false); prev != nil {
		t.Errorf("a cycle nothing has been written from was called part written (%v left)", left)
	}
	prev, left := mk(t, true)
	if prev == nil {
		t.Fatal("a paused first volume did not count as a part-written cycle; kura plan would abandon the cartridge")
	}
	if prev.Generation != 4 {
		t.Errorf("the unfinished cycle came back as generation %d", prev.Generation)
	}
	if len(left) != 2 {
		t.Errorf("%d volumes reported still to go, want both: %v", len(left), left)
	}
}

// TestACleanFinishIsNotAStop is a regression test for a bug this code had.
//
// The first version used signal.NotifyContext, whose stop function cancels the
// very context the watcher was waiting on — so the deferred stop at the end of
// every successful verify and restore tripped the watcher and announced a stop
// that had not happened. Two causes instead of one is what tells them apart.
func TestACleanFinishIsNotAStop(t *testing.T) {
	ctx, stop := cancelOnSignal(context.Background())
	if ctx.Err() != nil {
		t.Fatalf("the context was already cancelled before anything ran: %v", ctx.Err())
	}
	stop()
	if ctx.Err() == nil {
		t.Error("the context outlived the run, so a backend could still be handed it")
	}
	if errors.Is(context.Cause(ctx), errStopRequested) {
		t.Error("a run that finished normally reads as one the operator stopped")
	}
}

// TestASignalStopsTheRun is the other half: the signals named in DESIGN §11.1
// have to reach a verify or a restore, SIGHUP included, since a dropped session
// is an unexpected halt today and a clean stop beats dying dirty.
func TestASignalStopsTheRun(t *testing.T) {
	ctx, stop := cancelOnSignal(context.Background())
	defer stop()

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Skipf("cannot signal this process: %v", err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGHUP did not stop the run; a dropped session would still kill it dirty")
	}
	if !errors.Is(context.Cause(ctx), errStopRequested) {
		t.Errorf("the stop was not attributed to the signal: cause is %v", context.Cause(ctx))
	}
}

// TestAnAbandonedPauseIsRetiredAndNamed covers what -force leaves behind.
//
// Past the part-written refusal, the paused cycle is abandoned deliberately. The
// checkpoint then names a generation nothing can finish, and leaving it in place
// would print a note about an unresumable pause on every write for the rest of
// the archive's life. The cartridge is the real subject: its objects are still
// there with nothing indexing them, on media that never reclaims, so the operator
// is told which one while they can still go and look.
func TestAnAbandonedPauseIsRetiredAndNamed(t *testing.T) {
	dir := t.TempDir()
	wd := workdir.At(dir)
	cp := &archive.Checkpoint{
		Archive: "arch", Plan: "plan-4", Generation: 4, Volume: "LTO007",
		Objects: []archive.Placed{{Object: "data/a.bin"}, {Object: "data/b.bin"}},
	}
	if err := archive.SaveCheckpoint(wd.Resume(), cp); err != nil {
		t.Fatal(err)
	}

	// A plan for the same generation is the resume case and must leave it alone.
	retireAbandonedPause(wd, 4)
	if got, _ := archive.LoadCheckpoint(wd.Resume()); got == nil {
		t.Fatal("planning the same generation threw away the pause it was meant to resume")
	}

	// A plan for a later generation has abandoned it.
	retireAbandonedPause(wd, 5)
	got, err := archive.LoadCheckpoint(wd.Resume())
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Error("an abandoned pause survived the plan that replaced its cycle")
	}
	// Idempotent: a second plan must not fail on a checkpoint that is already
	// gone.
	retireAbandonedPause(wd, 6)
}
