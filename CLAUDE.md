# Working context

Read [DESIGN.md](DESIGN.md) before changing anything about the on-media format,
the catalog schema, or change detection. Those decisions were argued out and
several reversed at least once; the reasoning is recorded there so it does not
have to be rediscovered.

## Where this came from

Replacing a set of shell scripts that drove `tar -M` across ~20 LTO-6 cartridges.
The originals are at `/mnt/d/Misc/watanabe-backup/*.sh` — worth a look for what
the workflow actually is, not for what to keep.

Their fatal flaw shapes this whole project: `tar -M` holds one process open
across the entire multi-volume run, so a swap means the machine and the process
stay alive for days. That is why **every command here handles one volume and
exits**, with state in the catalog.

## Environment — the archive is not on this machine

Development happens here, under WSL on `/mnt/d`. The archive lives on a
different host (`Watanabe`), which is where `/storage` and the tape drive are.
**Nothing here can be run against real data**, and `/storage` does not exist
locally. The planner reaches the host as a static `linux/amd64` binary that gets
copied over and run there; that is why the standard-library-only rule is not
negotiable.

What *is* available locally, and is worth using before adding a measurement to
the design:

```
/mnt/d/Misc/watanabe-backup/planner/lists/*.files   202 MB, 13 volumes,
                                                    TSV: size, range, path
/mnt/d/Misc/watanabe-backup/planner/plan.json       478 MB, same data plus
                                                    hashes and item grouping
```

Those are the real 1.66 M paths and sizes from the 2026-09-02 scan. Every number
in the next section was computed from them with a few lines of Python, and any
new question about file distribution, tar overhead or packing behaviour can be
answered the same way without touching the host. Prefer the `lists` files —
they stream, and `plan.json` is 2.4× the size for fields you rarely need.

## The real archive

Every measurement below comes from the actual tree. Do not re-derive them;
do update them if the archive changes shape.

| | |
|---|---|
| Sources | `/storage`, `/storage_ssd` |
| Size | 31.24 TB, 1,656,854 files |
| Volumes needed | 13 LTO-6 (12 full + one at 62.3%) |
| Cartridges owned | ~20, so 7 spare |
| Stranded across 12 closed volumes | 756 kB — packing is not a problem |
| Cold scan | 34m50s · warm scan 1m40s |

### File size distribution — the fact that drives most decisions

| Bucket | Files | % files | Bytes | % bytes |
|---|---|---|---|---|
| < 1 KiB | 283,313 | 17.1% | 95.69 MB | 0.0% |
| 1 KiB – 1 MiB | 1,159,721 | 70.2% | 134.92 GB | 0.4% |
| 1 – 100 MiB | 189,710 | 11.5% | 4.08 TB | 13.1% |
| 100 MiB – 1 GiB | 17,429 | 1.1% | 3.30 TB | 10.6% |
| 1 – 10 GiB | 2,386 | 0.1% | 10.73 TB | 34.4% |
| > 10 GiB | 513 | 0.0% | 12.92 TB | 41.5% |

**87.3% of files hold 0.43% of the bytes.** That is why the tar threshold sits at
1 MB: it removes 1.44 M objects from the index while leaving every file that
carries meaningful data individually addressable. Tarring below 100 MB instead
would save ~12 min/volume and bury every photo in a container.

### Other measured facts

- Tar overhead on the sub-1MB set: **2.11 GB** with real paths (78% of paths
  exceed tar's 100-byte name field), 1.11 GB with ID-named members, versus
  **23 MB** for a manifest of random IDs.
- `-max-group` drives stranded space far more than the packing algorithm does:
  on a large-files-only tree, 200 GB groups strand 3.2% against 0.33% packing
  per file. On the real distribution it does not matter — the small-file tail
  tops every volume off.
- Baseline `-hash=all` read 569 GB. It does not recur; later cycles use `auto`.
- Excluding regenerable trees drops **541,951 files (33% of the count) for
  147 GB (0.5% of bytes)** and does not change the volume count. The point is
  write time, not space. Largest: immich thumbnails (376,436 files / 72.65 GB,
  regenerable from originals) and monitoring PVs (164,341 files / 74.67 GB,
  live Loki/Tempo/Prometheus state that rewrites itself).
- Only 3–5 leaf directories need more than one tarball, all of them 2 parts.

## State

**Built.** A cycle runs end to end: plan, write one volume at a time, verify,
restore.

| | |
|---|---|
| `internal/volume` + `filevol` | the backend seam and directory-as-volume — 16 tests |
| `internal/catalog` | records, history queries, `Lost`, `Tombstone`, per-object cipher — 25 tests |
| `internal/scan` | walk, exclusions, hashing, `Diff` — 38 tests |
| `internal/plan` | `Group`, `Layout`, `Pack` — 34 tests |
| `internal/archive` | `Write`, `Verify`, `Restore`, containers, both headers, volume identity — 34 tests |
| `cmd/kura` | `plan`, `write`, `verify`, `restore`, `volumes`, `history`, `at-gen` — 10 tests |
| `nix/e2e/scenarios` | seven NixOS VMs: real cartridges, swaps, ENOSPC, power cuts — see [TESTING.md](TESTING.md) |

157 tests. Exercised against a real 356-file tree across six volumes: written,
verified, restored byte-identical, then a flipped byte inside a container caught
and attributed to the one file it damaged, marked lost, and re-sourced by the
next plan.

**How a cycle goes.**

```sh
kura plan   -root /storage -catalog catalog.jsonl.gz -out plan.json
kura write  -plan plan.json -catalog catalog.jsonl.gz -mount /mnt/lto   # repeat per cartridge
kura verify -catalog catalog.jsonl.gz -mount /mnt/lto
```

`-mount` is where the medium is mounted and nothing more — a drive is one fixed
path, and the cartridge in it is whichever one the operator pushed in. Which
volume that is comes from the medium's own `kura.volume.json`, so loading the
wrong cartridge is refused rather than guessed at; a blank cartridge has no
identity yet and becomes the volume the plan names.

It names the mount point and never the device: kura does not mount block
devices, because from a device alone it cannot know the filesystem. `-device`
is the planned companion, for LTFS only, and it is not built. DESIGN §10.1.

**Where each decision lives.** This split was got wrong once and is worth
keeping straight:

- **`plan` decides the layout** — which files share a container, what each
  container is called, and how many bytes that will occupy. It has to, because
  `Item.Files` does not survive into the plan file: `Entries` is the
  authoritative list, and a writer reading a plan back can only work from it.
  Deciding twice is how a plan and a writer drift apart.
- **`archive` executes it** and names data objects, which are pure functions of
  the path.
- Packing works in **stored** bytes, grouping in **source** bytes. `-max-group`
  is a locality decision about what a restore must mount together; capacity is
  not.

**On-media layout**, all names relative so nothing can escape a volume:

```
kura.archive.json             archive identity and cipher — write-once
kura.volume.json              archive ID + volume ID, nothing else — write-once
generations/gNNNN/catalog.NNN.jsonl.gz   the index at that generation, twice
data/<path>                   files at or above the tar threshold
data/<path>.partNNN           one fragment of a split file
containers/gNNNN/<item>.partNNN.tar   everything below the threshold
```

**Not started.** Encryption, the LTFS backend, `reclaim`, `repair`, `forget`,
`erase`, parity.

## Conventions

- **Standard library only.** The binary has to copy onto a backup host and run.
  A dependency needs an argument. Test tooling lives outside the module —
  `tools/covcheck/` has its own `go.mod`, and the VM suite is Nix — so
  `go list -deps ./...` stays empty of anything but the standard library.
- Tests carry the reasoning for anything non-obvious, in the test name and a
  comment. Several existing tests document a bug that was actually hit —
  `TestDeletionAfterRewriteStaysDeleted`, `TestCTimeCatchesContentChange…`,
  `TestScanOverlappingRootsCountsOnce`. Do not "simplify" those away.
- `gofmt -w . && go vet ./... && go test ./...` before calling anything done.
- Comments explain *why*, not what. The what is in the code.
- **Coverage is a contract, not a report.** `.covcheck.json` holds a floor per
  package; `make cover-gate` enforces them and `covcheck ratchet` raises them.
  Floors only go up automatically — lowering one is a hand edit whose diff
  carries the argument.
- **Anything that only fails on real media gets an e2e scenario**, not a mock.
  A full medium, a swapped cartridge, a power cut mid-write and ctime at
  nanosecond resolution are all in `nix/e2e/scenarios/`, one VM each. Adding
  one is a single file — see [TESTING.md](TESTING.md).

## Things that will bite

- **A bug here destroys data.** That is the reason the file backend exists first
  and why the test suite reaches for failure paths rather than happy paths.
  Anything that touches the write path or the catalog needs the failure case
  tested, not just the success.
- **`Put` is atomic, `Sync` is durable.** Deliberate: an fsync per object would
  cost over an hour across 1.66 M objects, to guard against damage the mandatory
  verify pass catches anyway. Do not "fix" this by adding a sync to `Put`.
- **`Put` needs the size up front**, which is why a container is staged in a
  local file before being placed: a tar's encoded length is not knowable until
  it has been written. `-tar-max` bounds that staging file.
- **A volume must have room for its own index.** `PackOptions.MetadataReserve`
  holds it back. Without it, an oversized file split to fill the free space
  exactly leaves nowhere for the header and catalog, and the write fails on its
  own bookkeeping. The catalog grows with the archive, so the reserve is
  estimated from it rather than fixed.
- **Tar framing is not free.** 512-byte headers, padding, and an extended header
  for the 78% of real paths over 100 bytes — 2.11 GB across the archive.
  `plan.Layout.storedSize` counts it, and the estimate must never come in under
  the truth or a volume the plan said would fit will not.
- **Overwriting an object name is an error**, not a replace. On media that never
  reclaims space, a repeated name means the catalog lost track of what it stored.
- **Nothing mutable goes at the root of a volume.** LTFS does not modify a file;
  it appends the new bytes and abandons the old blocks. So a header field that
  changes costs a fresh copy every cycle and strands the last one. The root
  carries write-once identity, and anything per-cycle lives under
  `generations/gNNNN/`. This was got wrong once: the catalog was named by copy
  number, so a second generation on the same cartridge found the names taken and
  skipped them, leaving an index describing only the first cycle.
- **Object names are untrusted input.** They arrive from a catalog that may have
  been read off a volume someone else wrote. `filevol.resolve` refuses anything
  escaping the volume; any new backend needs the same.
- **A volume's identity comes off the medium, never from the drive.**
  `kura.volume.json` is the only thing that knows which cartridge is loaded, and
  `volume.Volume` has no `ID()` for that reason: a drive is one fixed mount
  point and holds whatever was pushed in. This was got wrong once — the file
  backend answered with its directory's base name, so the wrong-cartridge check
  compared the medium's header against a path the operator had chosen, and
  agreed with itself. `Reconcile` takes the expected volume from the plan; a
  blank cartridge becomes it, a written one is checked against it. DESIGN §3.1.
- **`-mount` must be a real mount point.** `write`, `verify` and `restore` check
  it against `/proc/self/mountinfo` and refuse a bare directory, because an
  unmounted path is the one operator error no later check can catch: the file
  backend is handed a directory either way, so the write succeeds and the
  catalog records a volume that is on no medium. A test or a local run against a
  temp directory needs `-allow-any-dir`, which is stripped from argv rather than
  registered so that no `-h` can advertise it. DESIGN §10.1.
- **Never rely on mtime alone.** See DESIGN §6. Ctime is nanoseconds, mtime is
  seconds, and that asymmetry is deliberate.
- **We correct no errors.** Every layer below either corrects silently or
  returns EIO, so `Health` cannot be answered through the filesystem and needs
  the device itself. DESIGN §8.1 has the three observable signals and what
  restic, borg and dar actually do — checked, and none of them corrects data
  either.

## Assumptions, not measurements

Everything in "The real archive" was measured. These were not, and each one has
a decision leaning on it. Confirm before treating any of them as settled — and
if one turns out wrong, follow the link to what has to move with it.

| Assumption | What rests on it | How to settle it |
|---|---|---|
| LTFS costs ~20–50 ms per file written | The 1 MB tar threshold. The *shape* of the curve is robust — it comes from the file-size distribution, not the constant — but the "12 min/volume" style figures do not | Time one small-file volume against one large-file volume of equal bytes |
| The drive answers SCSI `LOG SENSE` while LTFS holds the mount | Whether health can be sampled during a write, or only at mount and unmount | Try it; fall back to sampling at mount/unmount |
| Tape damage is a contiguous band across the medium | Whether parity must be striped across the tape or may sit in one region | Confirm the failure geometry before implementing parity at all |
| Read throughput degrades measurably on marginal media | Channel 3 of the degradation signals (DESIGN §8.1) | Needs a baseline first: collect MB/s per volume per scrub from the very first scrub, since a trend started late is worthless |
| LTO-6 usable capacity ≈ 2.5 TB | Planning arithmetic | Do not assume — LTFS reserves for the index partition. Read free space from the mounted filesystem, which also picks up compression gain |
| `/proc/self/mountinfo` reports LTFS as `fuse.ltfs` | How the LTFS backend is recognised without a `--backend` flag, and therefore which `Sync` runs (DESIGN §10.1) | `findmnt -o SOURCE,FSTYPE,MAJ:MIN /mnt/lto` against a mounted cartridge — one command, and it also says whether the source string happens to carry the device |
| LTFS flushes its index on demand, via the `ltfs.sync` virtual EA | `Volume.Sync` on tape. Without it the index only lands at unmount, so durability would require owning the mount | Set the EA on a mounted cartridge and check the index generation moved. `syscall.Setxattr` is in the standard library, so the mechanism costs no dependency |
| `/sys/class/scsi_tape/*/device` and `/sys/class/scsi_generic/*/device` resolve to the same SCSI address | Finding the drive's sg node without asking the operator, which is what keeps `-device` an override rather than a required flag | Compare the two symlinks on the host. The `scsi_generic` half is confirmed; the tape half needs a drive attached |

## Working style

Several decisions in DESIGN.md reversed at least once during design, and the
reversals are recorded next to the conclusions on purpose. Reopening one is
fine; reopening it *without new information* wastes the record. Where a number
would settle an argument, compute it from the `lists` files rather than
reasoning about it — that is how the tar threshold, the exclusion set and the
grouping rule were all decided, and each time the measurement contradicted the
intuition.

## Open

- **Symlinks, hardlinks and specials are scanned but never recorded.** `Diff`
  puts them in `Delta.Metadata` and nothing turns them into catalog records, so
  a restore does not recreate them. They carry no data, so this is a catalog
  gap rather than a data-loss one, but a restored tree is not yet faithful.
- **No way to feed in partly-filled volumes.** `plan.Pack` still takes them, but
  nothing supplies them: free space has to be read from a mounted volume rather
  than trusted from a file, and `plan` has no backend. Every cycle therefore
  starts fresh volumes, which wastes the tail of every cartridge.
- **Startup reconciliation is partial** (DESIGN §11). Blank, foreign, and
  "volume ahead" are handled; adopting a volume that is ahead and merging a
  divergent one both stop with an error rather than guessing, which is correct
  but not yet useful.
- **Module path.** Currently the placeholder `kura`. Needs a real one before
  publishing: `go mod edit -module <path>` plus a find/replace on imports.
- **Licence.** Apache-2.0 intended, for the patent grant; not yet added.
- **Retention horizon** — not needed until reclamation ships.
