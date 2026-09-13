package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"kura/internal/archive"
	"kura/internal/catalog"
	"kura/internal/plan"
	"kura/internal/volume"
)

func runVerify(argv []string) {
	argv, allowAnyDir := takeHiddenFlag(argv, hiddenAnyDir)
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	var (
		state    = registerState(fs)
		mount    = fs.String("mount", "", "where the loaded volume is mounted (required)")
		volumeID = fs.String("volume", "", "refuse to run unless the loaded volume is this one")
		noMark   = fs.Bool("no-mark", false, "report failures without recording them in the catalog")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `kura verify - read one volume back and check it against its digests

  kura verify -mount /mnt/lto

Checks whichever volume is loaded: which one that is comes from the medium's own
kura.volume.json, not from where it happens to be mounted. Pass -volume to make
that an assertion instead, for a script that means to scrub a particular
cartridge and should stop if the wrong one went in.

Reads every object the catalog says this volume holds and compares it against
the digest recorded when it was written. Nothing below this tool reports a
correction, so a read either works or it does not: this detects and reports,
and never repairs.

Failures are marked in the catalog, which is what makes the next plan re-source
those files from the live tree automatically. Exits non-zero if anything failed.

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
	cat, err := catalog.Load(wd.Catalog())
	if err != nil {
		fatal(err)
	}
	if err := requireMountPoint(*mount, allowAnyDir); err != nil {
		fatal(err)
	}
	ctx, stop := cancelOnSignal(context.Background())
	defer stop()
	backend, vol, err := openDrive(ctx, *mount, 0)
	if err != nil {
		fatal(err)
	}
	// Released on a live context, not the one above. A signal cancels ctx, and
	// unmounting is precisely the work that still has to happen after one — a
	// backend that does real I/O to let go of the medium would be handed a dead
	// context at the only moment it matters.
	defer backend.Release(context.Background())

	fmt.Fprintf(os.Stderr, "verifying %s ...\n", loadedVolume(ctx, vol, *volumeID))
	res, err := archive.Verify(ctx, vol, cat, !*noMark)
	if err != nil {
		fatal(err)
	}

	if !*noMark {
		if err := catalog.Write(wd.Catalog(), cat.Header, cat.Records); err != nil {
			fatal(err)
		}
	}

	fmt.Printf("\n=== VERIFY %s ===\n", res.Volume)
	fmt.Printf("  %s checked, %s\n", count(int64(res.Checked), "object"), plan.FormatBytes(res.Bytes))
	if res.Elapsed > 0 && res.Bytes > 0 {
		// Worth recording every time: a drive re-reading marginal media
		// delivers data more slowly, and that trend is the earliest free
		// warning there is. It only means something as a series.
		fmt.Printf("  %s at %s/s — record this; the trend is the signal, not the value\n",
			res.Elapsed.Round(time.Millisecond),
			plan.FormatBytes(int64(float64(res.Bytes)/max(res.Elapsed.Seconds(), 0.001))))
	}
	if len(res.Unclaimed) > 0 {
		fmt.Printf("\n  %s on the volume that no record accounts for:\n", count(int64(len(res.Unclaimed)), "object"))
		for _, n := range res.Unclaimed {
			fmt.Printf("    %s\n", n)
		}
		fmt.Printf("  Left alone. On media that never reclaims space this means the catalog\n")
		fmt.Printf("  lost track of a write, which is worth looking at rather than tidying away.\n")
	}
	if res.Stopped {
		fmt.Printf("\n  STOPPED before the whole volume was read, so this is not a verdict on %s.\n", res.Volume)
		fmt.Printf("  Nothing was changed and nothing is half done; run kura verify again to start over.\n")
		if res.Failed() {
			fmt.Printf("\n  What it did read found %s:\n", count(int64(len(res.Findings)), "problem"))
			for _, f := range res.Findings {
				fmt.Printf("    %s\n      via %s: %v\n", f.Path, f.Object, f.Err)
			}
		}
		os.Exit(exitPaused)
	}
	if !res.Failed() {
		fmt.Printf("\n  Clean.\n")
		return
	}

	fmt.Printf("\n  %s FAILED to read back as recorded:\n", count(int64(len(res.Findings)), "object"))
	for _, f := range res.Findings {
		fmt.Printf("    %s\n      via %s: %v\n", f.Path, f.Object, f.Err)
	}
	if !*noMark {
		fmt.Printf("\n  Marked lost in the catalog. The next plan will re-source them from the\n")
		fmt.Printf("  live tree for anything still there; run kura at-gen to see what history\n")
		fmt.Printf("  this costs. Try another drive first — head alignment varies, and a volume\n")
		fmt.Printf("  unreadable on the drive that wrote it may read on a different one.\n")
	}
	os.Exit(1)
}

func runRestore(argv []string) {
	argv, allowAnyDir := takeHiddenFlag(argv, hiddenAnyDir)
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	var (
		state    = registerState(fs)
		mount    = fs.String("mount", "", "where the loaded volume is mounted (required)")
		volumeID = fs.String("volume", "", "refuse to run unless the loaded volume is this one")
		into     = fs.String("into", "", "directory to restore into (required)")
		atGen    = fs.Int("at-gen", 0, "generation to reconstruct; default is the newest")
		noVerify = fs.Bool("no-verify", false, "skip digest checking as files are written")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `kura restore - write out what one volume holds

  kura restore -mount /mnt/lto -into /restore

Restores whichever volume is loaded, identified from its own kura.volume.json
rather than from where it is mounted; -volume makes that an assertion.

Restores the part of a generation that lives on the loaded volume, then says
what is still outstanding and on which volumes. Run it again with each of those
loaded. It never quietly produces a directory that looks complete: anything it
cannot produce is named, and the exit is non-zero.

Flags:
`)
		fs.PrintDefaults()
	}
	fs.Parse(argv)

	if *mount == "" || *into == "" {
		fmt.Fprintln(os.Stderr, "error: -mount and -into are both required")
		fs.Usage()
		os.Exit(2)
	}

	cat, err := catalog.Load(state.open().Catalog())
	if err != nil {
		fatal(err)
	}
	gen := *atGen
	if gen == 0 {
		gen = cat.MaxGen()
	}
	if gen < 1 {
		fatal(fmt.Errorf("the catalog is empty; there is nothing to restore"))
	}

	if err := requireMountPoint(*mount, allowAnyDir); err != nil {
		fatal(err)
	}
	ctx, stop := cancelOnSignal(context.Background())
	defer stop()
	backend, vol, err := openDrive(ctx, *mount, 0)
	if err != nil {
		fatal(err)
	}
	// Released on a live context, not the one above. A signal cancels ctx, and
	// unmounting is precisely the work that still has to happen after one — a
	// backend that does real I/O to let go of the medium would be handed a dead
	// context at the only moment it matters.
	defer backend.Release(context.Background())

	fmt.Fprintf(os.Stderr, "restoring generation %d from %s into %s ...\n", gen, loadedVolume(ctx, vol, *volumeID), *into)
	res, err := archive.Restore(ctx, vol, cat, gen, *into, !*noVerify)
	if err != nil {
		fatal(err)
	}

	fmt.Printf("\n=== RESTORE %s, generation %d ===\n", res.Volume, gen)
	fmt.Printf("  %s produced, %s\n", count(int64(res.Restored), "file"), plan.FormatBytes(res.Bytes))

	if len(res.Elsewhere) > 0 {
		ids := make([]string, 0, len(res.Elsewhere))
		for id := range res.Elsewhere {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		fmt.Printf("\n  Still to mount:\n")
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, id := range ids {
			fmt.Fprintf(w, "    %s\t%s\n", id, count(int64(res.Elsewhere[id]), "file"))
		}
		w.Flush()
	}
	if len(res.Incomplete) > 0 {
		fmt.Printf("\n  %s span volumes and are only partly written; they keep a %s\n",
			count(int64(len(res.Incomplete)), "file"), archive.PartialSuffix)
		fmt.Printf("  suffix until the volumes holding the rest have been restored:\n")
		for _, p := range res.Incomplete {
			fmt.Printf("    %s\n", p)
		}
	}
	if len(res.Lost) > 0 {
		fmt.Printf("\n  %s CANNOT be produced: the catalog already records them as lost.\n",
			count(int64(len(res.Lost)), "file"))
		for _, p := range res.Lost {
			fmt.Printf("    %s\n", p)
		}
	}
	if len(res.Failed) > 0 {
		fmt.Printf("\n  %s FAILED:\n", count(int64(len(res.Failed)), "file"))
		for _, f := range res.Failed {
			fmt.Printf("    %s\n      via %s: %v\n", f.Path, f.Object, f.Err)
		}
	}
	if res.Stopped {
		fmt.Printf("\n  STOPPED on request. Every file above is whole — each is written through a\n")
		fmt.Printf("  temporary and renamed — but the tree is not finished. Run kura restore again;\n")
		fmt.Printf("  it starts over, and overwrites what it already produced.\n")
		os.Exit(exitPaused)
	}
	if !res.OK() {
		fmt.Printf("\n  This restore is incomplete. Nothing above was silently skipped.\n")
		os.Exit(1)
	}
	if len(res.Elsewhere) == 0 && len(res.Incomplete) == 0 {
		fmt.Printf("\n  Generation %d is complete in %s.\n", gen, *into)
	}
}

func runVolumes(argv []string) {
	fs := flag.NewFlagSet("volumes", flag.ExitOnError)
	var (
		state   = registerState(fs)
		overdue = fs.String("overdue", "180d", "flag volumes not verified within this long; accepts 30d, 12h")
	)
	fs.Parse(argv)

	cat, err := catalog.Load(state.open().Catalog())
	if err != nil {
		fatal(err)
	}
	window, err := parseDuration(*overdue)
	if err != nil {
		fatal(fmt.Errorf("-overdue: %w", err))
	}

	if len(cat.Header.Volumes) == 0 {
		fmt.Println("no volumes recorded yet")
		return
	}
	inventory := append([]catalog.VolumeInfo(nil), cat.Header.Volumes...)
	sort.Slice(inventory, func(i, j int) bool { return inventory[i].ID < inventory[j].ID })

	fmt.Printf("\n=== VOLUMES (archive %s) ===\n", cat.Header.Archive)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "  VOLUME\tOBJECTS\tSIZE\tWRITTEN\tLAST VERIFIED\tLOST\n")
	var due []string
	for _, v := range inventory {
		lost := 0
		for _, r := range cat.OnVolume(v.ID) {
			if r.IsLost() {
				lost++
			}
		}
		verified := v.LastVerified
		if verified == "" {
			verified = "never"
			due = append(due, v.ID)
		} else if t, err := time.Parse(time.RFC3339, verified); err == nil && time.Since(t) > window {
			verified += "  (overdue)"
			due = append(due, v.ID)
		}
		lostCol := ""
		if lost > 0 {
			lostCol = fmt.Sprint(lost)
		}
		fmt.Fprintf(w, "  %s\t%d\t%s\t%s\t%s\t%s\n",
			v.ID, v.Objects, plan.FormatBytes(v.Bytes), orDash(v.Written), verified, lostCol)
	}
	w.Flush()

	if len(due) > 0 {
		// A full scrub cannot be one command that runs for days, so the useful
		// question is which volume to put in the drive next (DESIGN §8).
		fmt.Printf("\n  Most overdue first: %v\n", due)
		fmt.Printf("  kura verify -mount <where you loaded %s>\n", due[0])
	}
}

// loadedVolume names the cartridge in the drive, and stops if it is not the one
// the caller asked for.
//
// Verify and Restore work on whatever is loaded, so unlike write there is no
// plan to check against — want is optional and comes from -volume, for a script
// that meant a particular cartridge. A blank volume stops here rather than
// inside the operation, which would otherwise report a header error for what is
// really an empty drive.
func loadedVolume(ctx context.Context, vol volume.Volume, want string) string {
	id, err := archive.VolumeIDOn(ctx, vol)
	if err != nil {
		fatal(err)
	}
	switch {
	case id == "":
		fatal(fmt.Errorf("the loaded volume is blank; there is nothing on it to read"))
	case want != "" && id != want:
		fatal(fmt.Errorf("the drive holds volume %s but -volume asked for %s", id, want))
	}
	return id
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// parseDuration accepts Go durations plus a day suffix, because verification
// intervals are naturally months rather than hours.
func parseDuration(s string) (time.Duration, error) {
	if days, ok := cutSuffix(s, "d"); ok {
		d, err := time.ParseDuration(days + "h")
		if err != nil {
			return 0, err
		}
		return d * 24, nil
	}
	return time.ParseDuration(s)
}

func cutSuffix(s, suffix string) (string, bool) {
	if len(s) > len(suffix) && s[len(s)-len(suffix):] == suffix {
		return s[:len(s)-len(suffix)], true
	}
	return s, false
}
