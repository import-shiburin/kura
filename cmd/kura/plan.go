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
		catalogPath = fs.String("catalog", "", "catalog of what is already written (JSONL, optionally .gz; absent means first generation)")
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
		outPath     = fs.String("out", "plan.json", "where to write the machine-readable plan; - for none")
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

Read-only. Scans the roots, compares against the catalog of what is already
written, and bin-packs only the difference into volume-sized sets. Touches no
drive and writes nothing outside -out and -lists.

First run (no catalog yet), sizing the baseline:
  kura plan -root /storage -exclude-from media.exclude -hash=all -lists ./lists

Later runs, against the catalog the writer maintains:
  kura plan -root /storage -exclude-from media.exclude \
            -catalog catalog.jsonl.gz -allow-split

Flags:
`)
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
	fmt.Fprintf(os.Stderr, "scanning %s ...\n", strings.Join(scanning, " "))
	started := time.Now()
	files, stats, err := scan.Walk(parsed, ex, onErr)
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "scanned %d files (%s) in %s\n",
		stats.Files, plan.FormatBytes(stats.Bytes), time.Since(started).Round(time.Millisecond))

	cat, err := catalog.Load(*catalogPath)
	if err != nil {
		fatal(err)
	}
	if *catalogPath != "" && len(cat.Records) == 0 {
		fmt.Fprintf(os.Stderr, "note: catalog %s is empty or absent; treating this as generation 1\n", *catalogPath)
	}

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
		MetadataReserve: metadataReserve(*catalogPath, len(cat.Records)+len(toWrite), *copies),
	})
	p.Generation = gen
	p.Created = time.Now().UTC().Format(time.RFC3339)
	p.Finalize(delta, stats, items)

	writeReport(os.Stdout, p, delta, stats, *allowSplit)
	topDirs(toWrite, scan.CanonicalRoots(parsed), *topDepth, 15, os.Stdout)
	if warned > 50 {
		fmt.Fprintf(os.Stderr, "\n(%d more warnings suppressed)\n", warned-50)
	}

	if *outPath != "-" && *outPath != "" {
		if err := writePlanFile(p, *outPath); err != nil {
			fatal(err)
		}
		fmt.Printf("\nplan written to %s\n", *outPath)
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
	catalogPath := fs.String("catalog", "", "catalog to read")
	fs.Parse(argv)

	cat, err := catalog.Load(*catalogPath)
	if err != nil {
		fatal(err)
	}
	writeHistory(os.Stdout, cat)
}

func runAtGen(argv []string) {
	fs := flag.NewFlagSet("at-gen", flag.ExitOnError)
	catalogPath := fs.String("catalog", "", "catalog to read")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "kura at-gen [-catalog PATH] GENERATION\n\nFlags:\n")
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
	cat, err := catalog.Load(*catalogPath)
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
