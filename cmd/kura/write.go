package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"kura/internal/archive"
	"kura/internal/catalog"
	"kura/internal/plan"
	"kura/internal/volume"
	"kura/internal/volume/filevol"
	"kura/internal/workdir"
)

func runWrite(argv []string) {
	argv, allowAnyDir := takeHiddenFlag(argv, hiddenAnyDir)
	fs := flag.NewFlagSet("write", flag.ExitOnError)
	var (
		state    = registerState(fs)
		planPath = fs.String("plan", "", "plan to execute; default is the current one in the workdir")
		mount    = fs.String("mount", "", "where the loaded volume is mounted (required)")
		volumeID = fs.String("volume", "", "which volume of the plan to write; default is the first one not yet written")
		copies   = fs.Int("catalog-copies", archive.DefaultCatalogCopies, "how many times to write the catalog onto the volume")
		dryRun   = fs.Bool("dry-run", false, "read and digest everything, write nothing")
		quiet    = fs.Bool("quiet", false, "suppress per-object progress")
		recheck  = fs.Bool("verify-resumed", false,
			"re-read the objects a pause vouched for instead of trusting its record of them")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `kura write - place one volume's share of a plan

Writes exactly one volume and exits. Run it again, with the next cartridge
loaded, for the next one: state lives in the catalog, so the drive can be
powered off for weeks in between.

  kura write -mount /mnt/lto

-mount is where the medium is mounted, and nothing more: a drive is one fixed
path and the cartridge in it is whichever one you pushed in. Which volume that
is comes from the medium's own kura.volume.json, so loading the wrong cartridge
is refused rather than guessed at. A blank cartridge has no identity yet and
becomes the volume the plan asked for.

There is nothing to say about the catalog or the plan: both live in the
archive's workdir, which kura plan created and which the flagless form finds on
its own. -workdir or -archive picks between several archives on one host, and
a plan named with -plan is still checked against the archive and the cycle it
belongs to rather than taken on trust.

What goes in which container was settled by kura plan and is recorded in the
plan file, so a write that is interrupted and re-run places exactly the same
objects.

It can be stopped and picked up again. Ctrl-C, SIGTERM, SIGHUP or kura pause all
ask it to finish the object it is placing, flush the volume, write down what it
put there and exit 75; the cartridge can then be ejected and the host switched
off. Running it again resumes the same volume, and the objects the pause vouched
for are not read back. A run that died instead — killed, or a power cut — left
nothing to vouch for, so its resume reads the medium to find out what is on it.

Flags:
`)
		fs.PrintDefaults()
	}
	fs.Parse(argv)

	if *mount == "" {
		fmt.Fprintln(os.Stderr, "error: -mount is required")
		fs.Usage()
		os.Exit(2)
	}

	wd := state.open()
	from := *planPath
	if from == "" {
		from = wd.Plan()
	}
	p, err := loadPlan(from)
	if err != nil {
		fatal(err)
	}
	if note := planElsewhere(wd, p); note != "" {
		fmt.Fprintln(os.Stderr, note)
	}
	cat, err := catalog.Load(wd.Catalog())
	if err != nil {
		fatal(err)
	}
	if err := checkPlanCatalog(p, cat, wd.Path); err != nil {
		fatal(err)
	}

	pv := pickVolume(p, cat, *volumeID)
	if pv == nil {
		// Deletions still have to be recorded even when there is nothing to
		// write. A cycle that only removed files produces a plan with no
		// volumes at all, and it is still a generation.
		if n := cat.Tombstone(p.Deleted, p.Generation); n > 0 && !*dryRun {
			cat.Header.Generation = max(cat.Header.Generation, p.Generation)
			if err := catalog.Write(wd.Catalog(), cat.Header, cat.Records); err != nil {
				fatal(err)
			}
			fmt.Printf("recorded %s as deleted at generation %d\n", count(int64(n), "path"), p.Generation)
		}
		fmt.Println("every volume in this plan has already been written; nothing to do")
		return
	}

	if err := requireMountPoint(*mount, allowAnyDir); err != nil {
		fatal(err)
	}

	// Declaring the plan's capacity to the backend is what makes "the volume
	// filled up" reachable instead of silently spilling onto the host's disk.
	backend := filevol.New(pv.Capacity + pv.Reserved)
	if err := backend.Load(*mount); err != nil {
		fatal(err)
	}
	ctx := context.Background()
	vol, err := backend.Open(ctx)
	if err != nil {
		fatal(err)
	}
	defer backend.Release(ctx)

	// The cartridge check belongs to archive.Write, which compares what the
	// medium says it is against the volume the plan asked for. It cannot happen
	// here: opening the drive says nothing about which cartridge is in it, and
	// the check that used to sit at this point compared the plan against the
	// mount point's name, so it only ever agreed with itself.

	if err := ensureArchiveID(ctx, cat, vol, p, wd.Catalog(), *dryRun); err != nil {
		fatal(err)
	}

	// A checkpoint is read before anything is placed, but it is archive.Write
	// that decides whether it applies: it alone knows which cartridge is
	// actually loaded, and a pause is only good for the volume it names.
	resume, err := archive.LoadCheckpoint(wd.Resume())
	if err != nil {
		// Warned about rather than fatal, because a checkpoint is an optimisation
		// over a path that still works. A file this build cannot read — a newer
		// format, or one truncated by a full disk — must not stop a write that
		// can perfectly well resume by reading the medium back. Failing towards
		// distrust is the whole shape of DESIGN §11.1.
		fmt.Fprintf(os.Stderr, "warning: ignoring the paused write recorded in %s: %v\n", wd.Resume(), err)
		fmt.Fprintf(os.Stderr, "  This volume will be read back to find out what is on it, which is slower but no less safe.\n")
		resume = nil
	}
	pause := watchPause(wd)

	// Applied before archive.Write, so the catalog copies this volume carries
	// say what went away as well as what arrived. Persisting is left to the
	// save that follows a successful write; Tombstone is idempotent, so a run
	// that fails and is repeated re-applies them rather than losing them.
	tombstoned := cat.Tombstone(p.Deleted, p.Generation)

	written := int64(0)
	opt := archive.Options{
		Archive:       cat.Header.Archive,
		Plan:          p.ID,
		Generation:    p.Generation,
		CatalogCopies: *copies,
		StageDir:      wd.Stage(),
		DryRun:        *dryRun,
		Resume:        resume,
		VerifyResumed: *recheck,
		Pause:         pause.Asked,
		OnStart:       pause.Starting,
		OnObject: func(name string, size int64) {
			written += size
			if !*quiet {
				fmt.Fprintf(os.Stderr, "  %s  %s\n", plan.FormatBytes(size), name)
			}
		},
		OnProblem: func(path string, err error) {
			fmt.Fprintf(os.Stderr, "  ! %s: %v\n", path, err)
		},
	}
	label := ""
	if *dryRun {
		label = " (dry run, nothing written)"
	}
	fmt.Fprintf(os.Stderr, "writing %s, generation %d%s ...\n", pv.ID, p.Generation, label)
	started := time.Now()

	res, err := archive.Write(ctx, vol, pv, cat, opt)
	if err != nil {
		fatal(err)
	}
	elapsed := time.Since(started)

	if res.Paused {
		// Nothing is committed to the catalog here, and that is the point: the
		// records live in the checkpoint until the volume is finished, or
		// alreadyWritten would make the next run skip the very cartridge it is
		// meant to be finishing. DESIGN §11.1.
		if res.Checkpoint != nil {
			res.Checkpoint.Reason = pause.Reason()
			if err := archive.SaveCheckpoint(wd.Resume(), res.Checkpoint); err != nil {
				fatal(err)
			}
		}
		resumeWith := "kura write -mount " + *mount
		if *state.dir != "" {
			resumeWith += " -workdir " + wd.Path
		} else if *state.archive != "" {
			resumeWith += " -archive " + *state.archive
		}
		if *planPath != "" {
			resumeWith += " -plan " + *planPath
		}
		reportPause(res, elapsed, wd, resumeWith, *dryRun)
		// Released explicitly, because os.Exit runs no deferred call and a pause
		// is the one moment the medium has to become ejectable. Safe to call
		// twice by the interface's own contract, so the deferred one above
		// staying registered costs nothing.
		if err := backend.Release(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s is written and recorded, but the drive did not release it: %v\n",
				res.Volume, err)
		}
		os.Exit(exitPaused)
	}

	if !*dryRun {
		// The volume already carries this catalog; the local copy is a cache of
		// what the medium says.
		cat.Header.Generation = max(cat.Header.Generation, p.Generation)
		recordGeneration(cat, p.Generation, pv.ID, p.ID)
		if err := catalog.Write(wd.Catalog(), cat.Header, cat.Records); err != nil {
			fatal(err)
		}
		// The checkpoint has done its job once these records are in the catalog.
		// Only this volume's, though: a pause on another one is still somebody's
		// half-written cartridge.
		if resume.Describes(cat.Header.Archive, p.ID, p.Generation, res.Volume) {
			if err := archive.RemoveCheckpoint(wd.Resume()); err != nil {
				fatal(err)
			}
		}
	}

	reportWrite(res, elapsed, *dryRun, wd.Catalog())
	if tombstoned > 0 {
		fmt.Printf("  %s recorded as deleted at generation %d; the bytes stay on the volumes that hold them\n",
			count(int64(tombstoned), "path"), p.Generation)
	}
	if len(res.Problems) > 0 {
		os.Exit(1)
	}
}

// planElsewhere reports the announcement owed when a plan was made somewhere
// other than the workdir being written to.
//
// Reachable only through -plan: a plan taken from the workdir names the workdir
// it came from. It is said out loud rather than passed over because the
// workdir is what the command mutates and the pairing check below is not
// total — a plan made before its archive had an ID has nothing to compare.
func planElsewhere(wd workdir.Dir, p *plan.Plan) string {
	if p.Workdir == "" || p.Workdir == wd.Path {
		return ""
	}
	return fmt.Sprintf("writing into %s; this plan was made in %s", wd.Path, p.Workdir)
}

// checkPlanCatalog refuses a plan and a catalog that do not belong together.
//
// They were never independent arguments. A plan is a delta against one
// catalog: its volume IDs, its generation and its deletion list mean nothing
// beside a different one. A mismatched pair used to be accepted in silence and
// the result was unrecoverable — the plan's volume ID was stamped onto a blank
// cartridge next to the *other* archive's ID, the other catalog recorded files
// on a volume belonging to neither archive, and `verify` then called that
// cartridge Clean, because after the fact the whole arrangement is
// self-consistent. Nothing downstream could have caught it, which is why the
// check has to be here, before the first object is placed.
//
// Deliberately not a fingerprint of the catalog file. `write` rewrites the
// catalog after every volume, so volume 2 of a plan legitimately meets a
// different file than the plan was built from. What has to match is the
// lineage — the archive — and the cycle — the plan.
func checkPlanCatalog(p *plan.Plan, cat *catalog.Catalog, path string) error {
	if p.Archive != "" && cat.Header.Archive != "" && p.Archive != cat.Header.Archive {
		return fmt.Errorf("this plan was made against archive %s, but %s is archive %s; "+
			"they are not the same archive, so run kura plan against the catalog you mean to write",
			p.Archive, path, cat.Header.Archive)
	}

	// A plan made against a catalog that had no archive ID yet carries none to
	// compare, and that window — the first cycle of an archive — is exactly
	// where the two are easiest to mix up. The cycle closes it: whichever plan
	// wrote the first volume of this generation is named in the timeline, and
	// the rest of the cycle has to be that same plan. An interrupted first
	// write leaves no timeline entry, so resuming one is not caught here.
	//
	// Either side may be missing, and a missing one is a check that cannot run
	// rather than one that fails: a plan file from a build that recorded no ID
	// offers no evidence about which cycle it belongs to, and refusing it would
	// strand a cycle halfway through on nothing but a version difference.
	if p.ID == "" {
		return nil
	}
	for _, g := range cat.Header.Timeline {
		if g.Gen != p.Generation || g.Plan == "" || g.Plan == p.ID {
			continue
		}
		// Naming what the other plan already wrote is what makes the error an
		// instruction: it says which cartridge's plan to go and find.
		wrote := "that generation"
		if len(g.Volumes) > 0 {
			wrote = strings.Join(g.Volumes, ", ")
		}
		return fmt.Errorf("generation %d of %s was started by a different plan; "+
			"finish it with the plan that wrote %s, or run kura plan again for a new generation",
			p.Generation, path, wrote)
	}
	return nil
}

// ensureArchiveID settles which archive this catalog describes, and persists
// that before a single object is written.
//
// The order is the entire point. An archive ID is minted once for the archive;
// it is not something a volume write produces. While it was minted in memory
// and saved only after a whole volume succeeded, any interruption in between —
// a kill, a power cut, a medium that turned out too small — lost it, and
// because archive.Reconcile had already stamped the medium, the next run minted
// a different ID and refused the cartridge it had itself stamped. Permanently,
// and the window was the entire first volume of a new archive.
//
// Two things close it. The ID is written to the catalog immediately, so an
// interrupted run resumes with the identity it started with. And when the
// catalog has no ID at all but the loaded volume already declares one, the
// volume's is adopted rather than a second identity being minted for an archive
// that already has one — the medium is the authority (DESIGN §11), so a catalog
// lost entirely can still be repaired from any cartridge.
func ensureArchiveID(ctx context.Context, cat *catalog.Catalog, vol volume.Volume, p *plan.Plan, catalogPath string, dryRun bool) error {
	if cat.Header.Archive != "" {
		return nil
	}

	onMedium, err := archive.ArchiveIDOn(ctx, vol)
	if err != nil {
		return err
	}
	switch {
	case onMedium != "":
		cat.Header.Archive = onMedium
		fmt.Fprintf(os.Stderr, "adopting archive %s from the loaded volume; the catalog did not name one\n", onMedium)
	case p.Archive != "":
		// Third in line, and the order is the argument. The plan read this off
		// the catalog when it was made, so it is hearsay where the medium is a
		// primary source — but it is still evidence that this archive exists,
		// and minting a fresh ID for an archive that already has one is the
		// failure this whole function was written to prevent. Reachable when
		// the catalog was lost and the cartridge in the drive is blank.
		cat.Header.Archive = p.Archive
		fmt.Fprintf(os.Stderr, "adopting archive %s from the plan; the catalog did not name one\n", p.Archive)
	default:
		id, err := archive.NewArchiveID()
		if err != nil {
			return err
		}
		cat.Header.Archive = id
		fmt.Fprintf(os.Stderr, "new archive %s\n", id)
	}

	if dryRun {
		return nil
	}
	return catalog.Write(catalogPath, cat.Header, cat.Records)
}

// pickVolume chooses which volume of the plan to write.
func pickVolume(p *plan.Plan, cat *catalog.Catalog, want string) *plan.Volume {
	for _, pv := range p.Volumes {
		if want != "" {
			if pv.ID == want {
				return pv
			}
			continue
		}
		if !alreadyWritten(cat, pv.ID, p.Generation) {
			return pv
		}
	}
	if want != "" {
		fatal(fmt.Errorf("the plan has no volume %q", want))
	}
	return nil
}

// alreadyWritten reports whether this generation already put records on a
// volume, which is how the default volume choice advances between runs.
func alreadyWritten(cat *catalog.Catalog, id string, gen int) bool {
	for _, rec := range cat.OnVolume(id) {
		if rec.Gen == gen {
			return true
		}
	}
	return false
}

// recordGeneration keeps the timeline current, so history and a date-based
// restore have something to read, and names the plan that started the cycle so
// checkPlanCatalog can hold the rest of it to the same one.
func recordGeneration(cat *catalog.Catalog, gen int, volumeID, planID string) {
	for i := range cat.Header.Timeline {
		g := &cat.Header.Timeline[i]
		if g.Gen != gen {
			continue
		}
		// Backfilled rather than left alone, so a generation begun by a build
		// that did not record plans starts being checked at the next volume
		// instead of staying unchecked for the life of the cycle.
		if g.Plan == "" {
			g.Plan = planID
		}
		for _, v := range g.Volumes {
			if v == volumeID {
				return
			}
		}
		g.Volumes = append(g.Volumes, volumeID)
		return
	}
	cat.Header.Timeline = append(cat.Header.Timeline, catalog.GenerationInfo{
		Gen:     gen,
		Written: time.Now().UTC().Format(time.RFC3339),
		Volumes: []string{volumeID},
		Plan:    planID,
	})
}

func reportWrite(res *archive.Result, elapsed time.Duration, dryRun bool, catalogPath string) {
	fmt.Printf("\n=== WROTE %s ===\n", res.Volume)
	fmt.Printf("  %s in %d objects and %d containers, covering %d files\n",
		plan.FormatBytes(res.Bytes), res.Objects, res.Containers, len(res.Records))
	if elapsed > 0 {
		fmt.Printf("  %s at %s/s\n", elapsed.Round(time.Millisecond),
			plan.FormatBytes(int64(float64(res.Bytes)/max(elapsed.Seconds(), 0.001))))
	}
	if res.Adopted > 0 {
		fmt.Printf("  %d objects came from a pause and were taken as written, without being read back\n", res.Adopted)
	}
	if res.Resumed > 0 {
		fmt.Printf("  %d objects were already present from an interrupted run and were read back rather than rewritten\n", res.Resumed)
	}
	reportResumeNotes(res)
	if len(res.Problems) > 0 {
		fmt.Printf("\n  %s could not be written and are NOT recorded, so the next cycle will try again:\n",
			count(int64(len(res.Problems)), "file"))
		for _, p := range res.Problems {
			fmt.Printf("    %s: %v\n", p.Path, p.Err)
		}
	}
	if dryRun {
		fmt.Printf("\n  Dry run: nothing was written and %s is unchanged.\n", catalogPath)
		return
	}
	fmt.Printf("\n  Catalog updated: %s. The volume carries its own copy.\n", catalogPath)
}

// reportPause says where a stopped run got to and what finishes it.
func reportPause(res *archive.Result, elapsed time.Duration, wd workdir.Dir, resumeWith string, dryRun bool) {
	fmt.Printf("\n=== PAUSED %s ===\n", res.Volume)
	fmt.Printf("  %s in %d objects and %d containers, covering %d files\n",
		plan.FormatBytes(res.Bytes), res.Objects, res.Containers, len(res.Records))
	if elapsed > 0 {
		fmt.Printf("  %s at %s/s\n", elapsed.Round(time.Millisecond),
			plan.FormatBytes(int64(float64(res.Bytes)/max(elapsed.Seconds(), 0.001))))
	}
	if res.Adopted > 0 {
		fmt.Printf("  %d of those came from an earlier pause and were not read again\n", res.Adopted)
	}
	fmt.Printf("  %d of this volume's entries are still to go.\n", res.Remaining)
	reportResumeNotes(res)

	if dryRun {
		fmt.Printf("\n  Dry run: nothing was written, so there is nothing to resume.\n")
		return
	}
	// Said plainly because it is the one thing the operator most needs to know
	// and the one a pause could most easily be assumed not to give: the flush
	// happened before this was recorded, so the medium is safe to eject.
	fmt.Printf("\n  %s is flushed and safe to eject, and the host can be switched off.\n", res.Volume)
	fmt.Printf("  What this run placed is written down in %s, so resuming does not read it back.\n", wd.Resume())
	fmt.Printf("  Resume with: %s\n", resumeWith)
}

// reportResumeNotes prints what a resume could not take on trust. Each line is
// either an object that will be read again or a file the next cycle owes a write,
// so none of them is noise.
func reportResumeNotes(res *archive.Result) {
	for _, note := range res.ResumeNotes {
		fmt.Printf("  ! %s\n", note)
	}
}

func loadPlan(path string) (*plan.Plan, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p := new(plan.Plan)
	if err := json.Unmarshal(b, p); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if p.Format != plan.Format {
		return nil, fmt.Errorf("%s is format %q, this build writes %q", path, p.Format, plan.Format)
	}
	if p.Generation < 1 {
		return nil, fmt.Errorf("%s has no generation", path)
	}
	return p, nil
}

// openDrive loads a directory as the currently-inserted volume.
func openDrive(ctx context.Context, dir string, capacity int64) (*filevol.Backend, volume.Volume, error) {
	b := filevol.New(capacity)
	if err := b.Load(dir); err != nil {
		return nil, nil, err
	}
	vol, err := b.Open(ctx)
	if err != nil {
		return nil, nil, err
	}
	return b, vol, nil
}
