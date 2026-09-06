// Command kura plans and inspects an offline archive.
//
// Every subcommand handles one volume's worth of work and exits, with state
// living in the catalog rather than in a long-running process. That is the
// whole reason this replaced a `tar -M` pipeline: a multi-volume run there held
// one process open across every cartridge swap, so the machine had to stay up
// for days.
package main

import (
	"fmt"
	"os"

	"kura/internal/workdir"
)

// Version is stamped at build time with -ldflags "-X main.Version=...".
var Version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	args := os.Args[2:]
	switch os.Args[1] {
	case "plan":
		runPlan(args)
	case "write":
		runWrite(args)
	case "verify":
		runVerify(args)
	case "restore":
		runRestore(args)
	case "volumes":
		runVolumes(args)
	case "history":
		runHistory(args)
	case "at-gen":
		runAtGen(args)
	case "version", "-version", "--version":
		fmt.Println("kura", Version)
	case "help", "-h", "-help", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "error: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `kura %s - plan and inspect an offline archive

Usage:
  kura <command> [flags]

Commands:
  plan      work out which files go on which volume, and write a plan
  write     place one volume's share of a plan, then exit
  verify    read one volume back and check it against its digests
  restore   write out what one volume holds, for a given generation
  volumes   the inventory: what exists, and what is overdue a check
  history   print the generation timeline from the catalog
  at-gen    print the tree as it stood at one generation
  version   print the version

Run "kura <command> -h" for a command's flags.

A cycle is: plan once, then write once per volume, swapping media in between.

  kura plan  -root /storage
  kura write -mount /mnt/lto      # once per cartridge, unchanged each time

Every command handles one volume and exits, with state in the archive's workdir
under %s, so the drive can be powered off for weeks between
cartridges. Nothing there is yours to name: -workdir and -archive exist only to
pick between archives when a host holds more than one.
`, Version, workdir.Root())
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
