package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"kura/internal/archive"
	"kura/internal/catalog"
	"kura/internal/plan"
	"kura/internal/scan"
	"kura/internal/workdir"
)

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func runPlan(argv []string) {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	var (
		roots       stringList
		excludeFrom stringList
		excludeVCS  = fs.Bool("exclude-vcs", true, "skip version-control metadata")
		state       = registerState(fs)
		capacityStr = fs.String("capacity", "2.5TB", "usable capacity of a blank volume (LTO-6 native is 2.5TB)")
		headroomStr = fs.String("headroom", "1%", "space held back on every volume; accepts a size or a percentage")
		maxGroupStr = fs.String("max-group", "200GB", "largest subtree kept together on one volume")
		blanks      = fs.Int("blanks", -1, "blank volumes available; -1 for as many as needed")
		allowSplit  = fs.Bool("allow-split", false, "let a file span volumes to fill a hole nothing else fits")
		splitMinStr = fs.String("split-min", "100GB", "smallest hole worth splitting a file into")
		hashMode    = fs.String("hash", "auto", "sampled hashing: auto (match candidates only), all, or none")
		matchMode   = fs.String("match", "full", "how far to prove two files share content: full, sample, or off")
		ignoreCTime = fs.Bool("ignore-ctime", false, "do not treat a ctime change as reason to verify a file")
		ignoreInode = fs.Bool("ignore-inode", false, "do not treat an inode change as reason to verify a file")
		prefix      = fs.String("prefix", "LTO", "volume ID prefix")
		startIndex  = fs.Int("start", 1, "first number for newly allocated volumes")
		outPath     = fs.String("out", "", "extra copy of the machine-readable plan; - to write none at all")
		force       = fs.Bool("force", false, "replace the workdir's plan even when its cycle is unfinished")
		listsDir    = fs.String("lists", "", "directory to write per-volume file lists into")
		topDepth    = fs.Int("top-depth", 2, "path depth for the largest-directories report")
		copies      = fs.Int("catalog-copies", archive.DefaultCatalogCopies, "catalog copies each volume will carry; only used to reserve room for them")
		tarBelow    = fs.String("tar-below", "1MB", "bundle files smaller than this into tar containers; 0 to store every file separately")
		maxTar      = fs.String("tar-max", "1GB", "largest amount of source data in one container")
		jobs        = fs.Int("jobs", runtime.NumCPU(), "parallel readers for sampled hashing")
		quiet       = fs.Bool("quiet", false, "suppress per-path warnings")
	)
	fs.Var(&roots, "root", "root to scan, repeatable; PATH or PATH=CANONICAL to record a snapshot under the live path")
	fs.Var(&excludeFrom, "exclude-from", "file of exclusion patterns, one per line (repeatable; lists are combined)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `kura plan - work out which files go on which volume

Touches no drive. Scans the roots, compares against what the archive already
holds, and bin-packs only the difference into volume-sized sets. The plan it
produces is left in the workdir, where kura write picks it up.

  kura plan -root /storage -exclude-from media.exclude -allow-split

The first run has no archive to compare against and creates one: %s/<archive>,
holding that archive's catalog and plans and nothing an operator has to name.
Later runs find it again on their own, and -workdir or -archive picks between
several on one host.

Sizing a baseline, before committing any media:
  kura plan -root /storage -exclude-from media.exclude -hash=all -lists ./lists

Flags:
`, workdir.Root())
		fs.PrintDefaults()
	}
	fs.Parse(argv)

	if len(roots) == 0 {
		fmt.Fprintln(os.Stderr, "error: at least one -root is required")
		fs.Usage()
		os.Exit(2)
	}

	capacity := mustSize(*capacityStr, "-capacity")
	headroom := mustHeadroom(*headroomStr, capacity)
	maxGroup := mustSize(*maxGroupStr, "-max-group")
	splitMin := mustSize(*splitMinStr, "-split-min")

	switch *hashMode {
	case "auto", "all", "none":
	default:
		fatal(fmt.Errorf("-hash must be auto, all, or none"))
	}

	match := scan.MatchFull
	switch *matchMode {
	case "full":
		match = scan.MatchFull
	case "sample":
		match = scan.MatchSample
	case "off":
		match = scan.MatchOff
	default:
		fatal(fmt.Errorf("-match must be full, sample, or off"))
	}
	if *hashMode == "none" {
		match = scan.MatchOff
	}

	warned := 0
	onErr := func(path string, err error) {
		warned++
		if !*quiet && warned <= 50 {
			fmt.Fprintf(os.Stderr, "warn: %s: %v\n", path, err)
		}
	}

	var patterns []string
	for _, path := range excludeFrom {
		p, err := scan.LoadPatterns(path)
		if err != nil {
			fatal(err)
		}
		patterns = append(patterns, p...)
	}
	ex := scan.NewExcluder(patterns, *excludeVCS)

	parsed := make([]scan.Root, 0, len(roots))
	for _, spec := range roots {
		r, err := scan.ParseRoot(spec)
		if err != nil {
			fatal(fmt.Errorf("-root: %w", err))
		}
		parsed = append(parsed, r)
		if r.As != "" {
			fmt.Fprintf(os.Stderr, "reading %s, recording it as %s\n", r.Scan, r.As)
		}
	}

	scanning := make([]string, len(parsed))
	for i, r := range parsed {
		scanning[i] = r.Scan
	}
	// The last thing before the walk, and the first thing that touches disk.
	// Every argument has been checked by now, so a bad flag does not leave a
	// workdir behind; and a cold scan of the real archive is 35 minutes, which
	// is a long time to wait to be told the cycle in progress is not finished.
	wd := state.create()
	cat, err := catalog.Load(wd.Catalog())
	if err != nil {
		fatal(err)
	}
	if prev, left := unfinishedCycle(wd, cat); prev != nil && !*force {
		fatal(fmt.Errorf("generation %d is only part written: %s still to go (%s)\n"+
			"  finish it with kura write, or pass -force to abandon it and plan a new generation",
			prev.Generation, count(int64(len(left)), "volume"), strings.Join(left, ", ")))
	}
	if len(cat.Records) == 0 {
		fmt.Fprintf(os.Stderr, "note: %s holds nothing yet; treating this as generation 1\n", wd)
	}

	fmt.Fprintf(os.Stderr, "scanning %s ...\n", strings.Join(scanning, " "))
	started := time.Now()
	files, stats, err := scan.Walk(parsed, ex, onErr)
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "scanned %d files (%s) in %s\n",
		stats.Files, plan.FormatBytes(stats.Bytes), time.Since(started).Round(time.Millisecond))

	delta := scan.Diff(files, cat, scan.Options{
		Match:       match,
		Jobs:        *jobs,
		IgnoreCTime: *ignoreCTime,
		IgnoreInode: *ignoreInode,
	}, onErr)
	toWrite := delta.ToWrite()

	if *hashMode == "all" && len(toWrite) > 0 {
		fmt.Fprintf(os.Stderr, "hashing %d files so the next run can detect renames ...\n", len(toWrite))
		hashes := scan.HashAll(toWrite, *jobs, onErr)
		for _, f := range toWrite {
			h, ok := hashes[f.Path]
			if !ok {
				continue
			}
			f.Hash = h
			delta.HashedFiles++
			delta.HashedBytes += scan.SampleBytes(f.Size)
		}
	}

	// The generation has to be known before grouping: it is part of every
	// container's name, so that a later cycle chunking the same directory
	// cannot collide with this one.
	gen := cat.MaxGen() + 1
	lay := plan.Layout{
		Generation:   gen,
		TarBelow:     mustSize(*tarBelow, "-tar-below"),
		MaxContainer: mustSize(*maxTar, "-tar-max"),
	}
	items := plan.Group(toWrite, maxGroup, lay)

	// No existing volumes are passed in yet. The packer still takes them, but
	// the free space on a partly-filled volume has to be read from the volume
	// itself rather than trusted from a file, which needs a mounted backend
	// (DESIGN §1, and the LTO-6 capacity assumption in CLAUDE.md).
	p := plan.Pack(items, nil, plan.PackOptions{
		Capacity:        capacity,
		Headroom:        headroom,
		MaxBlanks:       *blanks,
		AllowSplit:      *allowSplit,
		SplitMin:        splitMin,
		NamePrefix:      *prefix,
		StartIndex:      *startIndex,
		Layout:          lay,
		MetadataReserve: metadataReserve(wd.Catalog(), len(cat.Records)+len(toWrite), *copies),
	})
	p.Generation = gen
	p.Created = time.Now().UTC().Format(time.RFC3339)
	// What this plan was computed against, so the writer does not have to be
	// told again and cannot be told wrong (DESIGN §10.2). Archive comes from
	// the workdir rather than the catalog: a first cycle has no catalog yet,
	// and the workdir under the root is already named by the archive it holds.
	// The plan's own ID borrows the archive generator because both want the
	// same thing — 16 random bytes — and neither is derived from its contents.
	p.Workdir = wd.Path
	p.Archive = wd.Archive()
	if p.ID, err = archive.NewArchiveID(); err != nil {
		fatal(err)
	}
	p.Finalize(delta, stats, items)

	writeReport(os.Stdout, p, delta, stats, *allowSplit)
	topDirs(toWrite, scan.CanonicalRoots(parsed), *topDepth, 15, os.Stdout)
	if warned > 50 {
		fmt.Fprintf(os.Stderr, "\n(%d more warnings suppressed)\n", warned-50)
	}

	// "-out -" is a dry look at the delta and writes nothing anywhere, which is
	// the one case where suppressing the workdir's copy is right: it means the
	// operator asked what a cycle would do, not for a cycle.
	if *outPath == "-" {
		fmt.Printf("\nno plan written (-out -)\n")
		return
	}

	// Otherwise the workdir's copy comes first and unconditionally, because it
	// is the one kura write reads. A plan the operator has to carry to the next
	// command is the thing this interface exists to remove.
	if err := writePlanFile(p, wd.Plan()); err != nil {
		fatal(err)
	}
	fmt.Printf("\nplan written to %s\n", wd.Plan())
	// Echoed with the same workdir selection this run used, because a plan made
	// somewhere other than the default root is one the next command has to be
	// pointed at again.
	where := ""
	if *state.dir != "" {
		where = " -workdir " + wd.Path
	}
	fmt.Printf("next: kura write -mount <where you loaded a cartridge>%s\n", where)
	if *outPath != "" {
		if err := writePlanFile(p, *outPath); err != nil {
			fatal(err)
		}
		fmt.Printf("copy written to %s\n", *outPath)
	}
	if *listsDir != "" {
		if err := writeLists(p, *listsDir); err != nil {
			fatal(err)
		}
		fmt.Printf("per-volume file lists written to %s/\n", *listsDir)
	}
}

func runHistory(argv []string) {
	fs := flag.NewFlagSet("history", flag.ExitOnError)
	state := registerState(fs)
	fs.Parse(argv)

	cat, err := catalog.Load(state.open().Catalog())
	if err != nil {
		fatal(err)
	}
	writeHistory(os.Stdout, cat)
}

func runAtGen(argv []string) {
	fs := flag.NewFlagSet("at-gen", flag.ExitOnError)
	state := registerState(fs)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "kura at-gen [flags] GENERATION\n\nFlags:\n")
		fs.PrintDefaults()
	}
	fs.Parse(argv)

	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	gen, err := parseGen(fs.Arg(0))
	if err != nil {
		fatal(err)
	}
	cat, err := catalog.Load(state.open().Catalog())
	if err != nil {
		fatal(err)
	}
	// Non-zero exit when the answer has holes, so a scripted restore check
	// fails loudly rather than acting on a tree that only looks complete
	// (DESIGN §5.1).
	if writeStateAt(os.Stdout, cat, gen) > 0 {
		os.Exit(1)
	}
}

// unfinishedCycle reports the plan already in the workdir when it still has
// volumes to write, together with which ones.
//
// While the operator named the plan file, replacing one was their own doing.
// Now that kura owns it, a second `kura plan` would overwrite the only copy of
// a cycle that has cartridges on the shelf and more to write. No bytes are
// lost — files that were never written are still new, so the next cycle picks
// them up — but the volumes already written are stranded part way through a
// plan nothing can finish, and the cartridges they were filling are wasted.
//
// Part written is the whole condition, and both halves matter. A plan nothing
// has been written from is free to replace — that is planning twice before
// committing any media, which is an ordinary thing to do and is how an operator
// reconsiders. A plan every volume of which is written is a finished cycle and
// has nothing left to strand.
//
// A plan this build cannot read is treated as nothing to protect. It cannot be
// executed either, so refusing on it would wedge the workdir with no way out
// but deleting a file the operator is no longer supposed to be handling.
func unfinishedCycle(wd workdir.Dir, cat *catalog.Catalog) (*plan.Plan, []string) {
	prev, err := loadPlan(wd.Plan())
	if err != nil {
		return nil, nil
	}
	var left []string
	written := 0
	for _, pv := range prev.Volumes {
		if alreadyWritten(cat, pv.ID, prev.Generation) {
			written++
			continue
		}
		left = append(left, pv.ID)
	}
	if written == 0 || len(left) == 0 {
		return nil, nil
	}
	return prev, left
}

func writePlanFile(p *plan.Plan, path string) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(p); err != nil {
		return err
	}
	return w.Flush()
}

func writeLists(p *plan.Plan, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, v := range p.Volumes {
		if err := writeVolumeList(v, filepath.Join(dir, v.ID+".files")); err != nil {
			return err
		}
	}
	return nil
}

// writeVolumeList emits a tab-separated list the writer can consume directly:
// size, byte range, then path. Whole files carry a range of "-".
func writeVolumeList(v *plan.Volume, path string) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	w := bufio.NewWriter(f)

	fmt.Fprintf(w, "# volume %s\tused %s\tfree %s\n", v.ID, plan.FormatBytes(v.Used), plan.FormatBytes(v.Free()))
	fmt.Fprintf(w, "# size\trange\tpath\n")
	for _, e := range v.Entries {
		rng := "-"
		if e.Split() {
			rng = fmt.Sprintf("%d:%d/part%d-of-%d", e.Offset, e.Offset+e.Length, e.Part, e.OfParts)
		}
		fmt.Fprintf(w, "%d\t%s\t%s\n", e.Size, rng, e.Path)
	}
	return w.Flush()
}

// metadataReserve estimates the room every volume needs for the things it says
// about itself: its header, and the catalog copies that make it readable with
// no external database.
//
// The catalog is written to every volume and grows with the archive, so this
// cannot be a constant. It is measured from the catalog already on disk where
// there is one, and from a per-record estimate for the records this cycle will
// add, then given half again as slack — under-reserving stops a write partway
// through a cartridge, while over-reserving costs a fraction of a percent of a
// volume.
func metadataReserve(catalogPath string, records, copies int) int64 {
	if copies < 1 {
		copies = archive.DefaultCatalogCopies
	}
	var size int64
	if fi, err := os.Stat(catalogPath); err == nil {
		size = fi.Size()
	} else {
		// No catalog yet, so estimate the whole thing. About 64 bytes per
		// record once gzipped, measured against catalogs of the real archive.
		size = int64(records) * 64
	}
	reserve := (size*int64(copies))*3/2 + (1 << 20)
	return max(reserve, plan.DefaultMetadataReserve)
}
