package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"kura/internal/archive"
	"kura/internal/catalog"
	"kura/internal/plan"
	"kura/internal/volume"
	"kura/internal/volume/filevol"
)

func runWrite(argv []string) {
	fs := flag.NewFlagSet("write", flag.ExitOnError)
	var (
		planPath    = fs.String("plan", "plan.json", "plan to execute")
		catalogPath = fs.String("catalog", "", "catalog to read and update (required)")
		drive       = fs.String("drive", "", "directory standing in for the loaded volume (required)")
		volumeID    = fs.String("volume", "", "which volume of the plan to write; default is the first one not yet written")
		copies      = fs.Int("catalog-copies", archive.DefaultCatalogCopies, "how many times to write the catalog onto the volume")
		dryRun      = fs.Bool("dry-run", false, "read and digest everything, write nothing")
		quiet       = fs.Bool("quiet", false, "suppress per-object progress")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `kura write - place one volume's share of a plan

Writes exactly one volume and exits. Run it again, with the next cartridge
loaded, for the next one: state lives in the catalog, so the drive can be
powered off for weeks in between.

  kura write -plan plan.json -catalog catalog.jsonl.gz -drive /mnt/lto/LTO001

The directory named by -drive stands in for the loaded medium, and its base name
is the volume's identity — writing to a volume the plan does not expect is
refused rather than guessed at.

What goes in which container was settled by kura plan and is recorded in the
plan file, so a write that is interrupted and re-run places exactly the same
objects.

Flags:
`)
		fs.PrintDefaults()
	}
	fs.Parse(argv)

	if *catalogPath == "" || *drive == "" {
		fmt.Fprintln(os.Stderr, "error: -catalog and -drive are both required")
		fs.Usage()
		os.Exit(2)
	}

	p, err := loadPlan(*planPath)
	if err != nil {
		fatal(err)
	}
	cat, err := catalog.Load(*catalogPath)
	if err != nil {
		fatal(err)
	}

	pv := pickVolume(p, cat, *volumeID)
	if pv == nil {
		// Deletions still have to be recorded even when there is nothing to
		// write. A cycle that only removed files produces a plan with no
		// volumes at all, and it is still a generation.
		if n := cat.Tombstone(p.Deleted, p.Generation); n > 0 && !*dryRun {
			cat.Header.Generation = max(cat.Header.Generation, p.Generation)
			if err := catalog.Write(*catalogPath, cat.Header, cat.Records); err != nil {
				fatal(err)
			}
			fmt.Printf("recorded %s as deleted at generation %d\n", count(int64(n), "path"), p.Generation)
		}
		fmt.Println("every volume in this plan has already been written; nothing to do")
		return
	}

	// Declaring the plan's capacity to the backend is what makes "the volume
	// filled up" reachable instead of silently spilling onto the host's disk.
	backend := filevol.New(pv.Capacity + pv.Reserved)
	if err := backend.Load(*drive); err != nil {
		fatal(err)
	}
	ctx := context.Background()
	vol, err := backend.Open(ctx)
	if err != nil {
		fatal(err)
	}
	defer backend.Release(ctx)

	if vol.ID() != pv.ID {
		fatal(fmt.Errorf("the loaded volume is %s but the plan expects %s; load the right cartridge or pass -volume %s",
			vol.ID(), pv.ID, vol.ID()))
	}

	if err := ensureArchiveID(ctx, cat, vol, *catalogPath, *dryRun); err != nil {
		fatal(err)
	}

	// Applied before archive.Write, so the catalog copies this volume carries
	// say what went away as well as what arrived. Persisting is left to the
	// save that follows a successful write; Tombstone is idempotent, so a run
	// that fails and is repeated re-applies them rather than losing them.
	tombstoned := cat.Tombstone(p.Deleted, p.Generation)

	written := int64(0)
	opt := archive.Options{
		Archive:       cat.Header.Archive,
		Generation:    p.Generation,
		CatalogCopies: *copies,
		StageDir:      filepath.Dir(*catalogPath),
		DryRun:        *dryRun,
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

	if !*dryRun {
		// The volume already carries this catalog; the local copy is a cache of
		// what the medium says.
		cat.Header.Generation = max(cat.Header.Generation, p.Generation)
		recordGeneration(cat, p.Generation, pv.ID)
		if err := catalog.Write(*catalogPath, cat.Header, cat.Records); err != nil {
			fatal(err)
		}
	}

	reportWrite(res, elapsed, *dryRun, *catalogPath)
	if tombstoned > 0 {
		fmt.Printf("  %s recorded as deleted at generation %d; the bytes stay on the volumes that hold them\n",
			count(int64(tombstoned), "path"), p.Generation)
	}
	if len(res.Problems) > 0 {
		os.Exit(1)
	}
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
func ensureArchiveID(ctx context.Context, cat *catalog.Catalog, vol volume.Volume, catalogPath string, dryRun bool) error {
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
		fmt.Fprintf(os.Stderr, "adopting archive %s from %s; the catalog did not name one\n", onMedium, vol.ID())
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
// restore have something to read.
func recordGeneration(cat *catalog.Catalog, gen int, volumeID string) {
	for i := range cat.Header.Timeline {
		g := &cat.Header.Timeline[i]
		if g.Gen != gen {
			continue
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
	if res.Resumed > 0 {
		fmt.Printf("  %d objects were already present from an interrupted run and were read back rather than rewritten\n", res.Resumed)
	}
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
