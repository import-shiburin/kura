package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"kura/internal/archive"
	"kura/internal/plan"
	"kura/internal/workdir"
)

// exitPaused is what a run that stopped on request returns.
//
// Not 0: a script reading success would advance to the next cartridge and leave
// this one half written. Not 1: a pause is not a failure and should not page
// anybody. 75 is EX_TEMPFAIL — try again later — which is the meaning wanted,
// and it is the same code whether a signal or the flag in the workdir asked for
// the stop, which a signal-derived 130 could not be. DESIGN §11.1.
const exitPaused = 75

// exitStoppedNow is what a second signal returns: the operator declined to wait
// for the object in flight, so this is a crash with a courtesy message.
const exitStoppedNow = 130

// pauseWatcher turns two triggers into the one predicate a long operation polls.
//
// Signals cover an operator at the terminal. The flag file covers the write that
// most needs pausing — hours into a volume, under a unit file or behind an SSH
// session that has already dropped, with no terminal to send a signal from.
type pauseWatcher struct {
	flag   string // the PAUSE file in the workdir; empty disables that half
	asked  atomic.Bool
	reason atomic.Value // string

	// looked throttles the stat of the flag file. Asked is called between every
	// object, and a volume of the real archive holds six figures of them.
	looked atomic.Int64

	mu      sync.Mutex
	current string // the object in flight, so a pause can say what it waits for
	size    int64
}

// watchPause installs the handlers and returns the watcher.
//
// A flag file left over from an earlier run is cleared rather than honoured. A
// pause request is about the run that was in flight when it was made; a new
// invocation is the operator asking for work, and a `write` that stopped dead on
// its first object because of yesterday's request would be a trap.
func watchPause(wd workdir.Dir) *pauseWatcher {
	p := &pauseWatcher{flag: wd.Pause()}
	if _, err := os.Stat(p.flag); err == nil {
		if err := os.Remove(p.flag); err == nil {
			fmt.Fprintf(os.Stderr, "clearing a pause request left over from an earlier run\n")
		}
	}

	ch := make(chan os.Signal, 2)
	// SIGHUP belongs here: a dropped session is an unexpected halt today, and a
	// clean stop is strictly better than dying dirty four hours into a volume.
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for sig := range ch {
			if p.asked.Load() {
				fmt.Fprintf(os.Stderr,
					"\nsecond %s: stopping now. Everything already on the medium is whole, but this\n"+
						"run recorded nothing, so the next one has to read it back to find out what is there.\n", sig)
				os.Exit(exitStoppedNow)
			}
			p.request(sig.String(), true)
		}
	}()
	return p
}

// Asked reports whether a stop has been requested.
//
// Cheap enough to call between every object: the signal half is an atomic read,
// and the flag file is looked at no more than once a second.
func (p *pauseWatcher) Asked() bool {
	if p.asked.Load() {
		return true
	}
	if p.flag == "" {
		return false
	}
	now := time.Now().UnixNano()
	last := p.looked.Load()
	if now-last < int64(time.Second) || !p.looked.CompareAndSwap(last, now) {
		return false
	}
	if _, err := os.Stat(p.flag); err != nil {
		return false
	}
	// Consumed here rather than left for the operator to tidy up, or the resume
	// would stop on its own first object.
	os.Remove(p.flag)
	p.request("kura pause", false)
	return true
}

// request records the first stop request and acknowledges it.
//
// The acknowledgement names the object in flight because the stop lands at the
// next boundary, and for a 10 GiB file that is minutes away — a run that went
// quiet instead would look hung.
func (p *pauseWatcher) request(reason string, bySignal bool) {
	if p.asked.Swap(true) {
		return
	}
	p.reason.Store(reason)
	again := ""
	if bySignal {
		again = "; press Ctrl-C again to stop now"
	}
	fmt.Fprintf(os.Stderr, "\n%s: pausing after %s%s\n", reason, p.inFlight(), again)
}

// Starting is the writer's OnStart hook.
func (p *pauseWatcher) Starting(name string, size int64) {
	p.mu.Lock()
	p.current, p.size = name, size
	p.mu.Unlock()
}

func (p *pauseWatcher) inFlight() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == "" {
		return "the object in flight"
	}
	return fmt.Sprintf("%s (%s)", p.current, plan.FormatBytes(p.size))
}

// Reason is what asked for the stop, for the checkpoint.
func (p *pauseWatcher) Reason() string {
	if r, ok := p.reason.Load().(string); ok {
		return r
	}
	return ""
}

// cancelOnSignal is the whole of what verify and restore need.
//
// Neither has anything to commit, so neither needs a checkpoint: verify only
// reads, and restore writes every file through a temporary and a rename, so a
// stop leaves a correct subset of the tree and a re-run simply does it again.
// The context is checked between objects, so the file in flight still finishes.
func cancelOnSignal(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	// Deliberately not signal.NotifyContext, and the cause is deliberately not
	// context.Canceled. The stop function NotifyContext hands back cancels the
	// very context its watcher waits on, so a deferred stop on a run that
	// finished normally announced a stop that had not happened. Two distinct
	// causes are what keep the cases apart, here and in a test.
	done := make(chan struct{})
	go func() {
		select {
		case sig := <-ch:
			fmt.Fprintf(os.Stderr, "\n%s: stopping after the object in flight; nothing is left half written\n", sig)
			cancel(fmt.Errorf("%w: %s", errStopRequested, sig))
		case <-done:
		}
	}()
	return ctx, func() {
		signal.Stop(ch)
		close(done)
		cancel(context.Canceled)
	}
}

// errStopRequested is the cause a context carries when a signal stopped the run,
// as opposed to the run simply being over. The two are told apart by cause and
// not by ctx.Err(), which says only that cancellation happened.
var errStopRequested = errors.New("stopped on request")

func runPause(argv []string) {
	fs := flag.NewFlagSet("pause", flag.ExitOnError)
	var (
		state = registerState(fs)
		clear = fs.Bool("clear", false, "withdraw a request instead of making one")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `kura pause - ask the write in progress to stop at the next object

  kura pause                 # ask
  kura pause -clear          # change your mind

A running kura write notices within a second, finishes the object it is placing,
flushes the volume, writes down what it put there, and exits 75. The cartridge
can then be ejected and the host switched off; kura write picks the same volume
up where it left off, and the objects the pause vouched for are not read again.

This exists because a signal is not always reachable: the write that most needs
pausing is the one four hours into a volume under a unit file, or behind an SSH
session that has already dropped. At a terminal, Ctrl-C does the same thing, and
a second Ctrl-C stops at once and accepts being treated as a crash.

The request is consumed by the run that honours it. It is not a mode: nothing
stays paused, and the next kura write runs to the end of the volume.

Flags:
`)
		fs.PrintDefaults()
	}
	fs.Parse(argv)

	wd := state.open()
	path := wd.Pause()

	if *clear {
		switch err := os.Remove(path); {
		case err == nil:
			fmt.Printf("withdrawn; the write in %s runs on\n", wd)
		case os.IsNotExist(err):
			fmt.Printf("nothing to withdraw: no pause was requested in %s\n", wd)
		default:
			fatal(err)
		}
		reportPaused(wd)
		return
	}

	body := fmt.Sprintf("requested %s\n", time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("asked the write in %s to stop at the next object boundary.\n", wd)
	fmt.Printf("  It may be a while: the wait is one object, which for a large file is minutes.\n")
	fmt.Printf("  Nothing happens if no write is running — the request is cleared when one starts.\n")
	reportPaused(wd)
}

// reportPaused says what is currently paused, which is the question an operator
// asks right after either half of this command.
func reportPaused(wd workdir.Dir) {
	cp, err := archive.ReadCheckpointHeader(wd.Resume())
	if err != nil || cp == nil {
		return
	}
	fmt.Printf("\n  %s is paused at generation %d: %s placed, %d still to go, as of %s\n",
		cp.Volume, cp.Generation, count(int64(cp.Count), "object"), cp.Remaining, cp.Paused)
	fmt.Printf("  Resume it by loading that cartridge and running kura write again.\n")
}
