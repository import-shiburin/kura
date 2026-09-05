// Command covcheck turns Go coverage profiles into a contract.
//
// The Go toolchain already measures coverage and already merges it: `go test
// -cover` and a `-cover`-instrumented binary both emit the same raw counter
// format, `go tool covdata` unions them, and `go tool cover` renders the
// result. None of that is worth reimplementing, and covcheck does not. What the
// toolchain has no opinion about is the part that keeps a coverage target
// alive over months:
//
//   - a floor per package, so one well-tested package cannot pay for a
//     neglected one and the number says *where* to work;
//   - a ratchet, so the floors only rise and a regression is a failed build
//     rather than a line in a report nobody reads;
//   - a worklist, so "87.4%" turns into the specific blocks to test next,
//     biggest first;
//   - exclusions that must carry a reason.
//
// It has no dependencies, and lives in its own module so that neither the
// binary's standard-library-only rule nor kura's own coverage denominator is
// affected by it.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const defaultConfigPath = ".covcheck.json"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "report":
		cmdReport(args)
	case "gate":
		cmdGate(args)
	case "ratchet":
		cmdRatchet(args)
	case "uncovered", "todo":
		cmdUncovered(args)
	case "lcov":
		cmdLCOV(args)
	case "merge":
		cmdMerge(args)
	case "init":
		cmdInit(args)
	case "help", "-h", "-help", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "error: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `covcheck - hold Go coverage to a per-package contract

Usage:
  covcheck <command> [flags]

Commands:
  report      per-package coverage against the floors in .covcheck.json
  gate        the same check, silent on success, exit 1 on any package below
  ratchet     raise every floor to what is measured now, so it cannot fall back
  uncovered   what to test next: uncovered blocks, biggest first
  lcov        emit an lcov tracefile for genhtml and other OSS viewers
  merge       union several profiles into one
  init        write a starting .covcheck.json

Profiles are read with -i, which takes a comma-separated list of files and
directories; a directory contributes every *.profile, *.out and *.cov inside
it. That is what lets one command consume unit coverage and a directory of
per-scenario e2e profiles in the same breath.
`)
}

// common holds the flags every subcommand needs, since all of them start by
// loading the same profile against the same config.
type common struct {
	inputs  string
	config  string
	root    string
	module  string
	exclude bool
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.inputs, "i", "coverage/merged.profile", "profiles to read; comma-separated files or directories")
	fs.StringVar(&c.config, "config", defaultConfigPath, "coverage contract")
	fs.StringVar(&c.root, "root", ".", "module root, for resolving source files")
	fs.StringVar(&c.module, "module", "", "module path to strip from profile names; default is read from go.mod")
	fs.BoolVar(&c.exclude, "apply-exclusions", true, "honour the config's exclude list")
}

// load reads the config and every requested profile.
func (c *common) load() (*Profile, *Config, *Excluder) {
	cfg, err := LoadConfig(c.config)
	if os.IsNotExist(err) {
		// A missing contract is not an error for the read-only subcommands:
		// covcheck has to be useful on a tree that has not adopted it yet,
		// otherwise nobody ever runs it the first time.
		fmt.Fprintf(os.Stderr, "note: %s not found; using zero floors\n", c.config)
		cfg = DefaultConfig()
	} else if err != nil {
		fatal(err)
	}

	ex := &Excluder{}
	if c.exclude {
		if ex, err = cfg.Excluder(); err != nil {
			fatal(err)
		}
	}

	paths, err := expandInputs(c.inputs)
	if err != nil {
		fatal(err)
	}
	if len(paths) == 0 {
		fatal(fmt.Errorf("no coverage profiles found in %q", c.inputs))
	}
	prof, err := LoadProfiles(paths)
	if err != nil {
		fatal(err)
	}
	if c.module == "" {
		c.module = modulePath(c.root)
	}
	return prof, cfg, ex
}

// expandInputs turns the -i list into concrete profile files.
//
// Directories expand rather than erroring because the e2e side produces one
// profile per scenario and the count grows with the suite; naming them
// individually on a command line would not survive a dozen scenarios.
func expandInputs(spec string) ([]string, error) {
	var out []string
	for item := range strings.SplitSeq(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		info, err := os.Stat(item)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			out = append(out, item)
			continue
		}
		entries, err := os.ReadDir(item)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			switch filepath.Ext(e.Name()) {
			case ".profile", ".out", ".cov":
				out = append(out, filepath.Join(item, e.Name()))
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// modulePath reads the module line from go.mod.
//
// Parsed by hand rather than with golang.org/x/mod: one line of a well-known
// format is not worth a dependency, even in a tools module.
func modulePath(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

func cmdReport(argv []string) {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	var c common
	c.register(fs)
	format := fs.String("format", "text", "text, markdown, or json")
	floors := fs.Bool("floors", true, "show each package's floor and whether it holds")
	fs.Parse(argv)

	prof, cfg, ex := c.load()
	rep := BuildReport(prof.Summarise(ex), cfg)

	switch *format {
	case "text":
		if *floors {
			rep.WriteText(os.Stdout)
		} else {
			rep.WriteTextNoFloors(os.Stdout)
		}
	case "markdown", "md":
		rep.WriteMarkdown(os.Stdout)
	case "json":
		if err := rep.WriteJSON(os.Stdout); err != nil {
			fatal(err)
		}
	default:
		fatal(fmt.Errorf("unknown format %q", *format))
	}
}

func cmdGate(argv []string) {
	fs := flag.NewFlagSet("gate", flag.ExitOnError)
	var c common
	c.register(fs)
	verbose := fs.Bool("v", false, "print the full table even when everything passes")
	fs.Parse(argv)

	prof, cfg, ex := c.load()
	rep := BuildReport(prof.Summarise(ex), cfg)

	if *verbose || !rep.Pass {
		rep.WriteText(os.Stdout)
	}
	if rep.Pass {
		return
	}

	fmt.Fprintln(os.Stderr)
	for _, row := range rep.Failures() {
		fmt.Fprintf(os.Stderr, "coverage: %s is %.2f%%, floor is %.2f%% (%d of %d statements)\n",
			row.Name, row.Percent, row.Threshold, row.Covered, row.Total)
	}
	fmt.Fprintln(os.Stderr, "\nrun `covcheck uncovered` for the blocks to test next")
	os.Exit(1)
}

func cmdRatchet(argv []string) {
	fs := flag.NewFlagSet("ratchet", flag.ExitOnError)
	var c common
	c.register(fs)
	round := fs.Float64("round", 0.1, "round floors down to a multiple of this")
	dry := fs.Bool("n", false, "report what would change without writing")
	fs.Parse(argv)

	prof, cfg, ex := c.load()
	changed := cfg.Ratchet(prof.Summarise(ex), *round)
	if len(changed) == 0 {
		fmt.Println("no floors to raise")
		return
	}
	for _, ch := range changed {
		fmt.Println("raise", ch)
	}
	if *dry {
		return
	}
	if err := cfg.Save(c.config); err != nil {
		fatal(err)
	}
	fmt.Printf("\nwrote %s\n", c.config)
}

func cmdUncovered(argv []string) {
	fs := flag.NewFlagSet("uncovered", flag.ExitOnError)
	var c common
	c.register(fs)
	limit := fs.Int("n", 40, "how many blocks to list; 0 for all")
	pkg := fs.String("pkg", "", "restrict to one package's import path prefix")
	fs.Parse(argv)

	prof, _, ex := c.load()
	gaps := Gaps(prof, ex, c.root, c.module)
	if *pkg != "" {
		var kept []Gap
		for _, g := range gaps {
			if strings.HasPrefix(g.File, *pkg) {
				kept = append(kept, g)
			}
		}
		gaps = kept
	}
	WriteGaps(os.Stdout, gaps, *limit)
}

func cmdLCOV(argv []string) {
	fs := flag.NewFlagSet("lcov", flag.ExitOnError)
	var c common
	c.register(fs)
	out := fs.String("o", "-", "output file; - for stdout")
	fs.Parse(argv)

	prof, _, ex := c.load()
	w, closeFn := create(*out)
	defer closeFn()
	if err := WriteLCOV(w, prof, ex, c.root, c.module); err != nil {
		fatal(err)
	}
}

func cmdMerge(argv []string) {
	fs := flag.NewFlagSet("merge", flag.ExitOnError)
	var c common
	c.register(fs)
	out := fs.String("o", "-", "output file; - for stdout")
	fs.Parse(argv)

	// Merging is a pure profile operation, so exclusions stay out of it: the
	// merged file is an input to other tools, and dropping blocks here would
	// bake one config's opinion into an artifact that outlives it.
	c.exclude = false
	prof, _, _ := c.load()

	w, closeFn := create(*out)
	defer closeFn()
	if err := prof.Write(w); err != nil {
		fatal(err)
	}
}

func cmdInit(argv []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	path := fs.String("config", defaultConfigPath, "where to write")
	force := fs.Bool("f", false, "overwrite an existing config")
	fs.Parse(argv)

	if _, err := os.Stat(*path); err == nil && !*force {
		fatal(fmt.Errorf("%s exists; -f to overwrite", *path))
	}
	if err := DefaultConfig().Save(*path); err != nil {
		fatal(err)
	}
	fmt.Printf("wrote %s; run `covcheck ratchet` to set floors from the current run\n", *path)
}

func create(path string) (io.Writer, func()) {
	if path == "-" {
		return os.Stdout, func() {}
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal(err)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		fatal(err)
	}
	return f, func() { f.Close() }
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "covcheck:", err)
	os.Exit(1)
}
