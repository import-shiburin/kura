# kura

An archiver for **offline, append-only media** — LTO tape, optical, or a shelf
of rotating disks — handled by a person on a schedule measured in weeks.

A *kura* (蔵) is the thick-walled storehouse a Japanese household kept apart from
the house: built to hold documents and valuables through fire, theft and time,
and opened deliberately rather than often. That is the relationship this tool
assumes you have with your media.

> **Status: early, but it works.** A full cycle runs: plan, write a volume at a
> time, verify, restore. Tape support is not built — the file backend is, and it
> is a real backend for an archive on rotating disks. Encryption is not built.

## What it is for

The model is a set of fixed-capacity volumes that are written once, never
reclaimed in place, mounted by hand, and left alone for weeks between handling.
Concretely:

- **Incremental forever.** After a baseline, a cycle writes only genuinely new
  content. Moved, copied and re-added files cost a catalog entry, not a rewrite.
- **Restorable from the volumes alone.** The catalog rides on every volume, so
  no external database is required to read an archive back.
- **Point-in-time restore, for free.** Space is never reclaimed, so every past
  state of the tree remains on the media whether or not anything remembers it.
  The catalog is a history rather than a snapshot, which is the only part that
  costs anything.
- **Optional encryption**, with names and structure hidden alongside contents.

## What it is not for

Online targets — S3, rsync destinations, live repositories. [restic][],
[borg][] and [rclone][] are excellent there and this design would be worse:
never reclaiming space is a liability rather than a feature, a catalog copied
onto every volume is pure overhead, and the manual-swap workflow is meaningless.

It also **corrects nothing**. Every layer below this one — the drive's ECC, the
disk's sector remapping — corrects silently and never says so; a read either
works or returns EIO with the data already gone. So the error story is detect,
report, migrate. A volume whose digests stop matching is a cartridge to copy
while it is still readable, not one to repair.

[restic]: https://restic.net
[borg]: https://borgbackup.readthedocs.io
[rclone]: https://rclone.org

## Layout

```
cmd/kura/              CLI entry point and report rendering
internal/catalog/      every version of every path ever written: a history,
                       not a snapshot
internal/scan/         walking the sources and working out what changed
internal/plan/         grouping the write set, deciding the on-media layout,
                       and packing it onto volumes
internal/archive/      turning a plan into objects, and objects back into files
internal/volume/       the seam between the archive and its media
  filevol/             a directory as a volume — the test harness, and the
                       real backend for archives on rotating disks
```

## Using it

```sh
# Work out what has to be written and onto which volumes. Read-only.
kura plan -root /storage -exclude-from media.exclude \
          -catalog catalog.jsonl.gz -out plan.json

# Write one volume, then exit. Run it again with the next cartridge loaded.
kura write -plan plan.json -catalog catalog.jsonl.gz -mount /mnt/lto

# Read a volume back and check it against the digests written with it.
kura verify -catalog catalog.jsonl.gz -mount /mnt/lto

# Which cartridge is most overdue a check?
kura volumes -catalog catalog.jsonl.gz

# Put a past state back. One volume at a time; it says what is still to mount.
kura restore -catalog catalog.jsonl.gz -mount /mnt/lto -into /restore -at-gen 7

# What was the archive at generation 7, and can all of it still be read?
kura at-gen -catalog catalog.jsonl.gz 7    # exits non-zero if anything is lost
kura history -catalog catalog.jsonl.gz
```

Every command handles one volume and exits, with state in the catalog, so the
drive can sit powered off for weeks between cartridges. That is the whole reason
this exists: the `tar -M` scripts it replaces held one process open across every
swap.

`-mount` is just where the medium is mounted — the same path every time. It is a
mount point and not a device: mount the medium yourself and point kura at it.
A path that is not a mounted filesystem is refused, because an unmounted one
would quietly fill the host's disk and record a volume that is on no medium.
Which cartridge is in it is read from the volume itself, so loading the wrong one
is refused instead of silently written over, and a blank one becomes whatever
volume the plan asked for.

Scanning a ZFS snapshot works with `-root /storage/.zfs/snapshot/x=/storage`,
which records the files under the live path so their identity survives the
snapshot being renamed each cycle.

## What a volume looks like

Nothing about the layout needs this tool to read it back:

```
kura.archive.json                what archive this is, and how it is encrypted
kura.volume.json                 which cartridge of it this one is
generations/g0001/catalog.000.jsonl.gz    the index, written twice per cycle
generations/g0001/catalog.001.jsonl.gz
data/storage/Movie/film.mkv      files large enough to be worth addressing
containers/g0001/storage/Docs.part001.tar    everything small, bundled for speed
```

Small files are bundled because the filesystem charges per file, not per byte —
20–50 ms each on LTFS, and 87% of a real archive's files hold 0.4% of its bytes.
The containers are ordinary tars.

## Documents

- [DESIGN.md](DESIGN.md) — the settled decisions and why they went that way.
- [CLAUDE.md](CLAUDE.md) — working context: measurements, conventions, state.
- [TESTING.md](TESTING.md) — coverage and the end-to-end VM suite.

## Building

```sh
go test ./...
CGO_ENABLED=0 go build ./cmd/kura
```

Standard library only, so the binary is static and copies onto a backup host
with nothing to install alongside it.

## Testing

```sh
make cover        # unit coverage, per package, against the floors it must hold
make cover-todo   # the biggest uncovered blocks, worst first
make e2e          # every end-to-end scenario, each in its own NixOS VM
make cover-all    # unit and end-to-end coverage merged into one number
```

The end-to-end suite boots a machine per scenario with real block devices
standing in for cartridges, so a full medium, a swapped cartridge and a power
cut are the real thing rather than a simulation. Its binary is built with Go's
coverage instrumentation, which puts driving the actual CLI in the same report
as the unit tests. [TESTING.md](TESTING.md) has the rest, including how to add
a scenario — it is one file, and nothing else changes.
