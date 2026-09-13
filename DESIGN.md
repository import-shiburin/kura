# Design

Decisions that are hard or impossible to change once bytes are on media, and the
reasoning behind each. Where a decision reversed during design, the reversal is
recorded — the wrong turn is usually more informative than the destination.

A presentation version of this lives at
<https://claude.ai/code/artifact/fcb71331-037f-4f28-907e-0a1f0b20d0b1>. This file
is the source of truth.

---

## 1. The core is not about tape

Strip the vocabulary away and the model is: **fixed-capacity, append-only,
offline volumes, mounted by hand, weeks between handling, no reclamation.**
Nothing in that says tape.

Everything above the `volume` package — planning, packing, the catalog,
generations, tombstones, encryption, verification scheduling — is medium-
agnostic and stays that way. A backend supplies six things: capacity, mount,
write, read, unmount, health.

| | LTO / LTFS | BD-R · M-DISC | Rotating disk |
|---|---|---|---|
| Volume size | 2.5–18 TB | 25–128 GB | 1–20 TB |
| Filesystem | LTFS | UDF | ext4 · exFAT · ZFS |
| Reclaim possible | no | no — write-once | yes, but the model declines to |
| Health source | MAM · TapeAlert | disc quality scan, if exposed | SMART |
| Cleaning concept | yes | none | none |
| Index check | `ltfsck` | none | `fsck` · `scrub` |

**Not generalized to online targets.** See README.

### 1.1 Two backends from day one

The file backend is not a second product, it is the **test harness**. A bug here
destroys data, and a tape drive cannot be meaningfully tested against — not in
CI, not on a laptop, and not at the failure paths that matter:

- flip a byte and confirm verify catches it
- declare a 40 MB volume and exercise the swap path
- kill a writer mid-object and confirm the next run reconciles
- construct a divergent catalog deliberately
- generate a random tree, mutate it across generations, restore at generation
  *N*, assert it matches what the tree was at *N* — thousands of cheap cycles

None of that is arrangeable against a cartridge on demand. So the file backend
is built first and the LTFS backend conforms to an interface that already works.

---

## 2. Two modes: plain and encrypted

**Plain is the default.** Mount the volume, browse, `cp` files out. Nothing
proprietary is needed to read it, ever. This is the property the whole thing was
built around and it does not get taken away by accident.

**Encrypted** means contents encrypted, names replaced by random IDs, structure
flattened, catalog encrypted.

An earlier draft had a middle rung — encrypted contents, legible names — on the
theory an operator might want to see what a volume holds without unlocking it.
It did not survive contact with the alternative: once scrambling costs 23 MB
across the whole archive (§4), leaving names in the clear protects nobody and
only creates a footgun. Two modes is also a much smaller test matrix.

### 2.1 Residual leak — do not oversell

Scrambling hides names. It does **not** hide object sizes or object count, both
of which sit in the filesystem index in the clear. An exact byte count
fingerprints a known release precisely, so an adversary with a candidate list
can still confirm what an archive contains. Closing that needs size padding,
which on write-once media costs capacity forever. Document the limit; offer
padding later as an explicit, expensive option if anyone asks.

---

## 3. Software encryption only

LTO-6 drives implement AES-256-GCM in hardware — keys over SCSI `SPOUT`, status
over `SPIN`, under 1% throughput cost, and the key never touches the cartridge.
Good technology, wrong choice here, for a reason unrelated to its quality:
**it exists on exactly one of the media this tool targets.** Building the
security model on a capability that vanishes when someone points the tool at a
hard disk means either two encryption paths to test, or a mode that silently
offers less protection than the user asked for.

LTFS's own encryption support argues the same way from the other side: marked
experimental, only `simple` and `flatfile` key plugins, and volumes written that
way cannot be imported into key-server-managed libraries.

Drive encryption remains available to anyone who wants it as an independent
layer underneath. The tool need not know.

### 3.1 The headers: archive-scoped and volume-scoped

Two objects per volume stay plaintext — enough for the tool to recognise its own
work and know how to proceed, and nothing more.

```json
// kura.archive.json — identical on every volume of the archive
{
  "magic":      "kura-archive",
  "format":     1,
  "archive":    "8f14e45f-…",
  "encrypted":  true,
  "cipher":     "aes-256-gcm",
  "kdf":        "argon2id",
  "kdf_params": { "salt": "…", "m": 262144, "t": 3, "p": 4 }
}

// kura.volume.json — this cartridge alone
{
  "magic":   "kura-volume",
  "format":  1,
  "archive": "8f14e45f-…",
  "volume":  "c9f0f895-…"
}
```

**Both are write-once, and the root holds nothing else.** This is forced by the
medium rather than chosen for tidiness. On LTFS a modification is not a
modification: the new bytes are appended at the current append point, the index
is repointed, and the old blocks are abandoned — not erased, not reclaimed, not
reused. So a header carrying a `generation` or a `written` date would cost a
fresh copy of itself on every cycle and strand the previous one, for a field
nobody can rewrite in place anyway.

Everything that changes is therefore versioned instead, under `generations/`:

```
kura.archive.json                          write-once identity
kura.volume.json                           write-once identity
generations/g0001/catalog.000.jsonl.gz     the index as of generation 1
generations/g0001/catalog.001.jsonl.gz
generations/g0002/catalog.000.jsonl.gz     …and as of generation 2
data/…                                     files at or above the tar threshold
containers/g0002/…                         everything below it
```

The medium's own format already works this way: LTFS writes each index with a
generation number, keeps the older ones, and can roll back to them. Following
it costs nothing and makes a cartridge readable at any generation it holds.

**`kura.volume.json` is the only answer to "which cartridge is this?"** Nothing
outside the volume knows. A drive is one fixed mount point — `/mnt/lto`, every
time — and what is in it is whatever the operator pushed in a moment ago; the
path is a property of the drive, not of the medium. There is no barcode to read
either, at least not through a filesystem, and a library that can report one is
an upgrade to this answer rather than a replacement for it.

This was got wrong once, and the shape of the mistake is worth keeping. The file
backend answered `ID()` with its directory's base name, so the wrong-cartridge
check compared the header against the mount path. Both sides came from the same
string, so the check could only agree with itself: it passed when the operator
had already named the mount point after the cartridge they believed was in, and
so confirmed that belief rather than testing it. Mounted at the obvious
`/mnt/lto`, every written cartridge was refused instead.

The check that replaces it compares the medium against **the plan**, and the two
cases divide cleanly:

| Cartridge | Where identity comes from |
|---|---|
| Written | its own header — and disagreeing with the plan is a wrong cartridge, refused |
| Blank | the plan, which stamps it; nothing else can name a fresh volume |

`verify` and `restore` have no plan, and want none: a scrub reads whatever is
loaded, and the header both names it and is the only thing to name it. `-volume`
turns that into an assertion for a script that meant a particular cartridge.

Losing `generation` and `written` from the header costs nothing that mattered.
They were there so twenty encrypted cartridges would not be twenty identical
bricks — and the generation directories answer that better, because how far a
cartridge has been written is now readable from a *listing*, with no read and
nothing unlocked. The activity timeline that §3.1 already accepted leaking is
leaked in exactly the same measure, by names instead of by fields.

**This was one object, and splitting it fixed a data-availability bug.** The
archive ID is minted once for the archive; it is not something a volume write
produces. While it lived only in the volume header, `write` minted it in memory,
`Reconcile` stamped it onto the medium before the first object, and the local
catalog was saved only after the whole volume succeeded. Any interruption in
between — a kill, a power cut, a medium that turned out too small — lost it. The
next run minted a different ID and refused, as foreign, the cartridge it had
itself stamped. Permanently, and the window was the entire first volume of a new
archive: hours on real hardware. Found by the end-to-end suite, which kills a
write mid-flight; no in-process test had reason to look.

Two things follow from the split. The identity is persisted to the catalog
before any object is written, so an interrupted run resumes with the identity it
started with. And because the identity is now an object of its own, **any volume
can hand it back** — a catalog that has lost the ID adopts the medium's rather
than minting a second identity for an archive that already has one, which is the
"medium is the authority" rule of §11 applied to identity itself.

The `archive` field is repeated in the volume header on purpose. It is redundant
against the archive header, and that redundancy is what lets the two be
cross-checked: a cartridge carrying pieces of two archives is *corrupt*, which
is a different problem from *foreign* and sends the operator somewhere else.

**The KDF salt is per archive.** §3.1 previously left this open, and the split
settles it: it has to be per archive, or an object migrated between volumes
could not be decrypted from its own record — which is exactly what §9.1 requires
of ciphertext migration. Being byte-identical on every volume is therefore a
property to preserve rather than an accident. The salt must be plaintext; a
passphrase cannot become a key without it.

The cipher and KDF identifiers are what let the format outlive its own
algorithms: **reading dispatches on what the header declares, writing always uses
the current default.** That matters more here than in most formats, because this
medium cannot be re-encrypted in place.

`generation` and `written` are the one judgement call. Strictly they leak an
activity timeline. Without them, twenty encrypted cartridges are twenty
identical bricks and you cannot tell which is newest without unlocking each —
which was the entire reason for a header.

The archive header is stamped before the volume header, so an interruption
between the two leaves a cartridge whose identity is known and whose volume
header is missing. `Reconcile` treats that as a half-finished stamp and
completes it, because the archive check has already established that the
identity matches.

One consequence worth costing before partly-filled volumes ship:
`PackOptions.MetadataReserve` currently holds back room for one catalog. A
cartridge appended to across *N* cycles carries *N* catalogs, each larger than
the last, so the reserve has to grow with expected appends rather than with
catalog size alone.

### 3.2 Random IDs, not a keyed hash

An HMAC of the path is tempting: deterministic, so a known file could be located
with only the key if the catalog were damaged. It leaks equality though — the
same object name recurring across volumes and generations tells an observer a
file did not change, which over a long-lived archive is a real traffic-analysis
surface. The recoverability it buys is close to nil, since the catalog is
mandatory in both modes and rides on every volume.

Random IDs, and flatten the directory structure too: scrambled names arranged in
the original tree still expose its shape.

### 3.3 Keys cannot be rotated

**A key cannot be rotated for data already on media.** Rotation works by
re-encrypting, and this archive never rewrites — which is the same property that
buys point-in-time restore. Rotation applies only to future generations, and a
compromised key stays valid for every volume already written.

The corollary: the passphrase is a single point of total loss, worse than in a
disk-backed tool because there is no second copy. `init` should push hard toward
a printed recovery code and refuse to proceed silently without one.

---

## 4. Tarring is for speed; IDs are for privacy

Small files are bundled into tar containers so the filesystem stops paying a
per-file cost. That threshold is **1 MB**, and it is **unchanged by the
encryption setting**.

An earlier draft argued that because a tarball hides its members' names,
encrypting should push the threshold up until everything was containerised. That
was wrong — it reached for the expensive instrument. Measured against the real
archive (1,443,034 files under 1 MB, 135.02 GB):

| Hiding 1.44 M filenames via… | Overhead | Of tarred data |
|---|---|---|
| tar, real paths | 2.11 GB | 1.6% |
| tar, ID-named members | 1.11 GB | 0.8% |
| manifest: random ID per file | **23 MB** | 0.017% |

~90× cheaper, because 78% of these paths exceed tar's 100-byte name field and
need a GNU long-name entry, while the path is already stored in the catalog and
the ID is the only new field.

So the two mechanisms separate cleanly:

- **Tar buys write speed** — 1.65 M objects collapse to 210 k. Costs 2.11 GB,
  which is 1.6% of the tarred data but **0.007% of the 31 TB archive**.
- **Random IDs buy privacy** — 23 MB, applied to every object.

They compose without interfering, and the encrypted and plain layouts stay
structurally identical, which is worth a great deal when the test suite covers
both. One optimisation: when encrypting, tar members can be named by their IDs
rather than paths, halving container overhead to 1.11 GB.

### 4.1 Grouping rule

A subtree that fits within the cap becomes one tarball. When it does not,
**merge consecutive siblings into runs of ≤ cap in name order**, recursing only
into children that individually exceed the cap.

The sibling merge is not optional. Without it, descending into fanout
directories (immich's hash tree) emits one tarball per child: a first attempt
produced 107,051 tarballs, 99,492 of them under 10 MB. With it, 4,298.

When a single directory's own files exceed the cap they chunk in name order.
Membership is fixed at write time and never revisited, so boundaries never shift
retroactively — new files go into new tarballs in a later generation. Names carry
the generation (`<dir>.g0003.part001.tar`) so a later chunking of the same
directory cannot collide.

---

## 5. The catalog is a history, not a snapshot

Space is never reclaimed, so the bytes for every past state stay on the media
whether or not anything remembers them. **Only the bookkeeping is optional**,
and it costs one line per change. So records are never removed, only tombstoned:

- `gen` — the generation that wrote this version
- `deleted_gen` — the generation in which the path stopped existing; zero means
  it is still there

A path may have several versions. Whether it **exists** is a property of its
newest version, not of any single record — an older version keeps `deleted_gen`
zero because its bytes are still the correct answer for any generation before
the edit.

That distinction is load-bearing. A file written, later rewritten, then deleted
leaves an untombstoned first version behind; reading liveness off individual
records would resurrect the path. This was a real bug in the planner, caught by
`TestDeletionAfterRewriteStaysDeleted`.

Deletion detection must also run over live paths only, or a file removed three
cycles ago is reported as newly deleted forever.

**Who writes the tombstone.** The scan finds deletions and the writer records
them, so the paths travel in the plan file: nothing else survives between the
two, and the scan's own `Delta` does not reach `write`. This was missing for
long enough to be worth naming — `deleted_gen`, `Tombstoned`, `GoneBy` and
`At` were all built and all tested against records whose `deleted_gen` the tests
set by hand, so the reading half looked complete while no producer existed at
all. `at-gen` listed files deleted generations ago and a restore of the newest
generation recreated them. Found by the end-to-end suite; every unit test that
touched tombstones supplied its own.

Applying them is **idempotent**, and that is load-bearing rather than tidy. A
deletion belongs to a generation, not to a cartridge, so there is no "first
volume" to hang it on; the writer applies the whole list on every volume of the
generation, and a cycle that only removed files produces a plan with no volumes
at all and still has to record them. Only the newest version of each path is
marked, per the rule above.

### 5.1 Unavailability is per version, not per path

When a volume is damaged the catalog starts lying: it says a path at generation
*N* lives at object *X* on volume *V*, and those bytes are gone. It has to be
able to say so, and at the right granularity.

That granularity is **the version, not the path**. A file written at generation
1 and rewritten at generation 5 has two records on two volumes; if only the
older volume is damaged, the file is unavailable for generations 1–4 and fine
from 5 onward. Marking the record rather than the path gives that for free,
because a record already covers a generation range.

```go
// Lost records that a version's bytes are no longer retrievable.
type Lost struct {
    Gen    int    // when this was recorded
    Reason string // "media" | "reclaimed"
    Detail string // "LTO007 unreadable during verify 2026-09-03"
}
```

**A lost record is never removed.** It still carries the size, the digest, the
generations it covered and the volume it was on — which is what lets `at-gen`
report *"1,247 objects unavailable"* instead of quietly returning a short tree,
and what lets the content be recognised if it ever turns up again.

The two reasons differ in a way that matters to whoever reads the report:
`reclaimed` was chosen, `media` was suffered. Never collapse them into one flag.
They also differ in reversibility — a reclaimed volume has been erased, whereas
a `media` marker is a *current assessment* and can be cleared if the object
later reads successfully. That is the payoff of the retry ladder in §8.1: a
different drive succeeds, the marker comes off.

Four consequences for the queries, and the first is the useful one:

1. **Diff treats a lost version as needing a write**, even when stat data
   matches. So marking something lost automatically re-protects it on the next
   cycle, for anything still on the source — self-healing that falls out of the
   existing incremental logic rather than needing its own machinery. It is what
   borg achieves by accident when a later backup happens to re-supply a chunk,
   except deliberate and scoped.
2. `At(gen)` returns lost records flagged rather than omitting them, so a
   point-in-time query can be honest about its holes.
3. `Lookup` still returns the version; callers check `Lost` themselves. The path
   does exist — it just cannot currently be produced.
4. **Restore fails loudly, per object, and exits non-zero.** Never a directory
   that looks complete.

Markers are set by `verify` when a read fails, by `reclaim` for history
deliberately not carried forward, and left in place by `repair` for whatever it
could not re-source.

---

## 6. Change detection

| tool | "unchanged" test | content identity |
|---|---|---|
| rclone | size + mtime | whole-file backend hash |
| restic | path + mtime + **ctime** + size + **inode** | SHA-256 per CDC chunk |
| borg | size + mtime_ns + **inode** | SHA-256 per chunk |
| rmlint / jdupes | size → partial hash → full hash → byte-for-byte | full verify before declaring a match |
| **kura** | size + mtime + **ctime** + **inode** | sampled digest, confirmed by full digest |

**Ctime and inode corroborate mtime.** Userspace can set mtime freely and plenty
of software does — package managers, archive extraction, `rsync -t`, torrent
clients, media managers. A file rewritten at the same size with its mtime put
back is invisible to a size-and-mtime check, so it would never be backed up
again and the loss surfaces at restore time. Restic has an open issue titled
exactly that ([#2179](https://github.com/restic/restic/issues/2179)).

Ctime is stored in **nanoseconds** while mtime is seconds, because the case it
exists to catch is a rewrite within the same second as the previous scan —
precisely where second granularity is blind. Storing it in seconds made both
guard tests skip.

**But a ctime mismatch triggers a read, not a rewrite.** This is where tape
economics diverge from a disk-backed tool. Restic and borg treat ctime
disagreement as "changed" and re-read, which costs almost nothing. Here a false
"changed" verdict costs a *write to media*, so the file is hashed to decide: a
`chmod -R` sweep must not rewrite the archive, and a file whose mtime was
restored after a real edit must not be missed.

`-ignore-ctime` and `-ignore-inode` relax it, named to match restic's flags.

### 6.1 Matching

Tiered size → sampled digest → full digest, the same shape rmlint and jdupes
use. Default confirms against a whole-file digest, following jdupes' rule of
never declaring a match on a partial hash alone — the sampled digest covers size
plus the first and last mebibyte, so two files differing only in the middle
collide, which for a move means restoring the wrong bytes under the right name.

The match pool includes tombstoned records, so content deleted generations ago
and re-added today costs a catalog line rather than a write.

### 6.2 Content-defined chunking: rejected

CDC (restic, borg) would dedupe *within* a modified file — editing 1 MB of a
40 GB image would store 1 MB rather than 40 GB. Rejected because reassembling a
file from chunks requires the tool, and the requirement here is that a volume be
restorable with standard utilities. Media files also tend to be replaced
wholesale rather than edited in place.

---

## 7. Snapshots

A run spanning days cannot read a live tree — files move underneath it and the
plan goes stale. Confirmed in practice: the first scan of `/storage` lost ~21
files mid-run to Loki/Tempo WAL churn; a second, faster scan lost 1. Exposure
scales with how long you are looking, and a write is measured in days.

`--root PATH=CANONICAL` reads from a snapshot and records under the live path.
Without it a ZFS snapshot's `.zfs/snapshot/<label>` prefix changes every cycle,
every file looks new, and the incremental scheme collapses. Exclusion patterns
match the canonical path, so one exclude file works whatever the snapshot is
called.

**On ZFS specifically:** ctime is fine (nanosecond timestamps in the znode).
Inode is fine for this purpose — object IDs are stable within a dataset, are
presented identically inside snapshots, and survive `zfs send`/`recv`. ZFS
recycles object IDs (disambiguated by a generation number `stat()` does not
expose) and each dataset has its own ID space, but neither produces a false
"unchanged" here, only extra files marked suspect. `-ignore-inode` covers the
noisy cycle after a pool rebuild.

`zfs diff` was considered and **rejected**: it reports renames explicitly and
would skip the tree walk, but a cold scan is ~35 min against a multi-day write —
about 1% — which does not justify a ZFS-specific path plus `zfs allow` setup.

---

## 8. Verification

| Tier | Cost | Catches |
|---|---|---|
| Cartridge memory | instant | drive-reported errors, load count, lifetime writes — from MAM, without touching media |
| Index check | seconds | `ltfsck`: index consistency, unclean unmounts |
| Full read + digest | ~4 h / volume | actual bit rot, against the digests the writer recorded |

A full scrub reads every byte — ~31 TB, roughly the 54 hours the write took. It
cannot be one command that runs for days; track `last_verified` per volume and
rotate, so the tool answers "which is most overdue?" Surface the **trend** in
soft error counts, not the current value: a volume degrading over three scrubs
is the signal, and it arrives long before a read fails.

TapeAlert flags are **cleared when the page is read**, so read once after every
operation and persist them or they are lost.

### 8.1 We correct nothing, and cannot

This tool sits on a filesystem — LTFS for tape, ext4 or similar for disk — and
every error correction that exists happens below it. The drive's Reed-Solomon
runs continuously and silently; the disk reallocates sectors without telling
anyone. **Those corrections are never propagated up.** A read either succeeds,
in which case we cannot tell whether it took one attempt or four hundred, or it
fails with EIO, by which point every layer beneath has already given up and the
data is gone.

So the tool's entire error story is *detect, report, migrate* — never repair.
Worth stating plainly in the README, because "backup software" invites the
opposite assumption.

The one exception, and the only correction capability that could ever exist
here, is **parity we add ourselves**, above the filesystem (see the table
below). Everything else in this section is about noticing in time.

#### Degradation is observable, but never through the filesystem

Three channels, and only the first is precise:

1. **Out-of-band device query.** SCSI `LOG SENSE` read/write error counters,
   TapeAlert, and MAM for tape; SMART for a disk. These carry the corrected-
   error counts the filesystem hides. They require the *device* alongside the
   mount — so a backend needs both paths, plus raw device access and usually
   elevated privileges. Until that is built, `Health` returns `StatusUnknown`
   for every real backend and `StatusDegrading` never fires. *Unverified:*
   whether the drive will answer LOG SENSE while LTFS holds it mounted; if not,
   read the counters at mount and unmount instead.

2. **Retry counts from our own verify pass.** Filesystem-level, free, needs no
   privileges. A volume that needed three retries this scrub and none the last
   is degrading, and we can see that without asking the device anything.

3. **Read throughput per volume, tracked across scrubs.** The most useful free
   signal, and the least obvious. A drive re-reading marginal media internally
   delivers data more slowly — the corrections are invisible but *the time they
   cost is not*. Same for a disk stalling on pending sectors. Recording
   effective MB/s per volume per scrub turns invisible corrections into a
   visible trend. Needs calibration against normal variance before it can raise
   an alarm, but it costs nothing to collect from the first scrub onward, and
   the history is worthless if collection starts later.

Which makes the scrub schedule load-bearing rather than hygienic: channels 2 and
3 only exist if verification runs regularly, and without any of the three there
is no early warning at all.

`StatusDegrading` is therefore a **migration trigger, not a repair signal**. A
volume whose numbers are worsening is still fully readable *today*, and copying
it to fresh media now is the whole remedy. Miss the window and the option is
gone. This is the media-retirement path in §9.1.

**Attribute, do not just count.** Errors across every cartridge mean a dirty or
dying drive; errors on one mean that cartridge. Opposite responses.

#### What can actually be done about a bad read

| | Cost | Recovers | Corrects? |
|---|---|---|---|
| Retry, re-seat, clean, **try a different drive** | free | transient errors — tension, alignment, debris. Head alignment varies between drives, so a tape unreadable on the drive that wrote it may read on another | no — retries the read |
| Intra-volume parity records (~5%) | 5% capacity | localized damage: a crease, a scratch, a bad patch. Not a lost cartridge | **yes** — the only real correction available |
| Cross-volume erasure coding | ~8% for one parity volume in 13 | total loss of any one volume — but rebuilding means reading *all* survivors: ~50 hours and 12 manual swaps | yes |
| **A second independent copy** | one more volume set | everything, in one mount | no — substitutes |

Parity works despite the filesystem because a read error is per-block: EIO on
one region does not stop us seeking past it and reading the rest, so the
readable remainder plus parity reconstructs the hole. That makes intra-volume
parity the only mechanism by which this tool could ever repair anything, which
raises it from a nice-to-have to the one deliberate gap in the design.

#### What the reference implementations actually do

Checked, because "surely restic solves this" is the obvious first thought:

- **restic** — `check --read-data` detects; `repair packs` and `repair snapshots`
  remediate. No parity, no redundancy, no error correction anywhere in the
  format. `repair snapshots` *removes* inaccessible data from snapshots: damage
  mitigation, not recovery. Its advice is to rerun the backup, since a later run
  may re-supply the missing blobs and heal older snapshots, and to go fix the
  hardware.
- **borg** — `check --repair` recovers what it can from segments with integrity
  errors and rebuilds the index, then replaces lost chunks with **same-size
  all-zero chunks**, with extract warning distinctly about them. A zero-patched
  file can be healed later if the lost chunks reappear via a subsequent backup.
  The documentation is blunt: *"Fixing corruptions does not mean recovering lost
  data… Repairing a repository means sacrificing some data for the sake of the
  repository as a whole."*
- **dar** — the maintainer **declined** to build parity into the format
  ([feature request #170][dar170], closed 2018), on the grounds that par2 already
  does it well and adjustably and embedding it would complicate the tool. dar
  does carry structural redundancy: duplicate slice and archive headers, and
  **catalog copies distributed throughout the archive**, so it survives a
  corrupted or missing slice at the metadata level.
- **In practice**, dar users pair it with par2 externally — [dar-backup][] ships
  **5% coverage** by default, "enough to fix localized bitrot."
- **For tape specifically**, the standard answer remains two copies. There is no
  established in-band redundancy scheme.

[dar170]: https://sourceforge.net/p/dar/feature-requests/170/
[dar-backup]: https://github.com/per2jensen/dar-backup

So nobody in this space corrects data errors in-format, which is a reassuring
confirmation rather than a gap to fill. Four things are worth taking from it:

**a. Replicate the catalog within a volume, not just across volumes.** Stolen
directly from dar. The catalog is already written to every volume, but once per
volume — so damage to the region holding it costs that volume's index. It is
small; write it two or three times at separated points on the medium. Nearly
free, and it protects the one structure whose loss makes everything else on the
volume unreadable.

**b. `kura repair --volume X` — re-source from the live tree.** This is the
mechanism restic and borg both fall back on ("rerun the backup and it may heal"),
except we can aim it. The catalog knows exactly which objects a given volume
holds, and the source is normally intact, so a damaged volume's contents can be
re-sourced from disk and rewritten to current media without a full backup run
and without reading the damaged volume at all. It is the reclamation machinery
(§9.1) pointed at a different trigger, so most of it is already designed. This
is the strongest recovery capability available here and it should be a named
command rather than folklore.

**c. Parity: opt-in, par2-style, ~5%.** Matches established practice and the
failure mode it addresses. Worth weighing the dar maintainer's objection — that
it complicates a tool for something an external utility already does well —
though two of his specific reasons (no temporary files, streaming over SSH) are
not constraints here, and on LTFS the recovery files are simply more objects on
the volume.

**d. Never silently produce a short tree.** Borrowed from borg's zero-chunk
behaviour, which is loud by design. A restore that cannot produce an object must
say so per object and exit non-zero — never quietly write a directory that looks
complete. Same principle as the catalog being honest about its holes (§9.1).

Recommended: build the **retry ladder** (it is free and recovers real transient
failures, and a second drive is the single highest-value thing in the list);
offer **intra-volume parity** as opt-in, since localized damage is the common
case and it needs no cross-volume reads; and **do not build erasure coding**.
Spending 50 hours and twelve swaps to rebuild one cartridge is worse than
keeping a second copy, which the tool already supports — a second archive UUID
over the same source, with its own volume set. Media failures are independent
between the two, and recovery is a single mount.

One thing already in place: the catalog is written to every volume, so the most
critical structure in the archive is the one thing that already has thirteen
copies.

Storing files as individual objects rather than one large container also bounds
the blast radius — damage costs the objects it touches, not everything after
them in a stream. Worth remembering when the tar threshold is next revisited.

*Unverified:* tape damage tends to be a contiguous band across the medium
(a crease, a scratch), so parity that is striped across the tape should help
where parity concentrated in one region would not. Confirm the geometry before
implementing parity, rather than assuming.

---

## 9. Retention and reclamation

Two commands, the split familiar from restic and borg's `forget`/`prune`:

- `kura forget` — catalog only. Instant, touches no hardware, reversible.
- `kura reclaim` — moves media.

**Retention is an operation, not a policy.** Nothing expires on a schedule,
nothing drops in the background. An archival tool that quietly discards history
while nobody is watching has the failure mode backwards. The newest generation
can never be forgotten.

Forgetting frees less than intuition suggests: an object dies only when *no*
retained generation references it, and an unchanged file is referenced by every
generation since it was written. So it frees superseded revisions and pre-window
deletions, not the bulk of an append-mostly archive.

### 9.1 Reclamation

Source the rewrite **from the backup source, not from the old volume**. The
catalog knows what the volume holds, so the old medium never has to be mounted
or still be readable, and disk out-reads tape by an order of magnitude.

The trap: re-sourcing reproduces only the archive's *current* state, so it is
not only deleted files that become unrecoverable but **every historical version
the volume held**. A consent prompt listing only deletions understates the
damage badly. This is the one operation where someone could destroy history
believing they were tidying up.

You only ever read from the old volume what you cannot get from the source, and
whatever you read must be **staged on disk** — a single drive cannot have both
volumes mounted. With two drives, stream directly.

Stage ciphertext verbatim; an object moving between volumes does not need
decrypting. This means the **cipher belongs per object in the catalog**, not
only in the volume header, so a blob written under an older algorithm can
migrate onto a newer volume without a re-encrypt.

**Two reasons to reclaim, wanting opposite defaults.** For capacity, keeping
history defeats the exercise — writing 2.41 TB to free a 2.5 TB cartridge gains
nothing; `forget` has already done the deciding. For media retirement, keep
everything and expect to consume an equivalent volume elsewhere. Ask which.

Ordering is a safety property: choose → price the options → classify casualties
→ stage → plan → write → **verify by digest (the gate)** → record the
reclamation as a catalog event → release. Erase is a separate explicit command,
so a mistake found an hour later is still recoverable.

Past generations must then report themselves as partially unavailable —
`at-gen 1` says "1,247 objects dropped in generation 12", never quietly returns
a short answer. The time-travel property is only worth having if the catalog is
honest about its holes.

Never reclaim while the source is degraded. It is a maintenance operation, not a
recovery one.

---

## 10. Interface

One binary, subcommands:

```
kura init     --source /storage --encrypt=contents
kura plan     --snapshot auto-2026-09-02
kura write    --volume LTO014        # one volume, then exits
kura verify   --volume LTO007        # or --overdue
kura restore  --at-gen 7 --into /restore

kura forget   --keep-last 6          # catalog only, reversible
kura reclaim  --volume LTO00001      # prices the options, then moves media
kura repair   --volume LTO007        # re-source a damaged volume from the tree
kura erase    --volume LTO00001      # the one irreversible step

kura volumes                         # inventory, free space, health, last verified
kura history                         # generations, dates, volumes
kura catalog  --rebuild-from-volume  # bootstrap with no local DB
```

**One volume per invocation, then exit.** State lives in the catalog, so the next
volume can be days later with the drive powered off in between — which is the
failure mode that made the old `tar -M` scripts painful. Emitting a shell script
was considered and dropped: once encryption, random IDs, catalog reconciliation
and digest verification are in scope, a script cannot express the work.
`--dry-run` covers the auditability a script would have bought.

For a workflow with day-long pauses, **being told when to act matters more than
having somewhere to look**. A pluggable notifier earns its place before any GUI.
When a GUI does arrive, a TUI fits better than a web app — the operator is at a
terminal beside the drive, and it avoids a browser dependency on a NAS reached
over SSH.

### 10.1 Reaching the medium: `-mount`, and `-device` for tape alone

`-drive` was the first name for the path a volume is mounted at, and it was
wrong in both directions. Every other flag names the value it takes — `-plan` a
plan file, `-catalog` a catalog, `-into` a directory — while `-drive` named the
hardware at the far end of its argument, and this document uses "drive" for the
machine throughout (§8.1: "try a different drive"). One word was carrying two
things, and the one it was attached to was the one it did not mean.

**`-mount` is where the loaded volume is mounted.** For a shelf of disks it is
the only flag there is. **`-device` is the drive itself**, and exists for LTFS
alone.

| Flags | Who mounts | On exit |
|---|---|---|
| `-mount /mnt/lto` | the operator | kura leaves the mount alone |
| `-device /dev/sg0` | kura, into a private directory | unmounts; `-eject` also unloads |

One rule covers it: **whoever mounted it unmounts it.** `-eject` alongside
`-mount` is refused rather than obeyed, because kura did not make that mount and
will not take it away.

**`-mount` is checked against the kernel's mount table**, and a path that is not
the root of a mounted filesystem is refused. This is the one input error nothing
downstream can catch. A mistyped path, or a cartridge nobody pushed in, leaves
an ordinary directory — and a directory is exactly what the file backend expects
to be handed, so every later check agrees with it: objects are written, the
catalog records the volume as stored, the command exits zero, and the archive
finds out at the next restore. By then the only evidence, that the path was not
a mount, is gone. The refusal names the filesystem the bytes would have landed
on, since "you were about to fill `/dev/sdb1`" is what tells an operator what
they actually did.

`/proc/self/mountinfo` is the authority rather than comparing `st_dev` against
the parent directory: a bind mount shares its device number with what it binds,
so the `st_dev` test would refuse a real mount, and mountinfo yields the
filesystem type in the same read — which is what §10.1 needs anyway to tell
`fuse.ltfs` from ext4.

The escape hatch is `-allow-any-dir`, and it is **not registered as a flag** —
it is stripped from argv before parsing, so no help text mentions it. That is
deliberate rather than coy. The check exists to catch an unmounted cartridge,
and an operator who meets the switch in a listing of flags has been invited to
defeat it; it is for the test suite and for a developer pointing a command at a
temp directory.

**kura never mounts a block device.** Given only a device it cannot know the
filesystem. `mount(2)` — which is all a standard-library-only binary has — takes
an explicit type; `-t auto` lives in mount(8) and libblkid, not in the syscall,
so detection would mean reimplementing superblock probing. The partition map
cannot stand in for it: MBR `0x83` and GPT `0FC63DAF-…` cover ext4, xfs and
btrfs alike, and a disk formatted whole has no map to consult at all. Guessing
wrong is not worth saving the operator one `mount`, so disks are adopt-only —
a decision, not a stage on the way somewhere.

Tape escapes this because **a tape is not a block device.** `/dev/nst0` is a
SCSI character device with no partition map and no superblock, and `ltfs` is a
FUSE daemon pointed at it. The filesystem is known before the flag is parsed,
which is the one case where kura may run a mount itself.

**Reading the fstype of an existing mount is a different act.** Refusing to
probe a raw device is refusing to guess; reading `/proc/self/mountinfo` for a
mount the kernel already made is reading a fact. kura needs that fact, because
`Sync` on `fuse.ltfs` is an index flush and on ext4 it is fsync — the wrong one
costs either an hour of fsyncs across 1.66 M objects, guarding against damage
the mandatory verify pass catches anyway, or an index that never reaches the
tape. *Unverified:* that mountinfo reports LTFS as `fuse.ltfs`; one
`findmnt -o SOURCE,FSTYPE,MAJ:MIN /mnt/lto` against a mounted cartridge settles
it.

**Which is why there is no `--backend` flag.** The sketch above carried one and
it is gone: `-mount` plus the fstype answers it, `-device` implies LTFS, and
there was never a third option to choose between.

**Finding the device from the mount does not work, and does not need to.** For a
block filesystem mountinfo gives a real `major:minor` and source; a FUSE mount
gets an anonymous `0:N` and a source string of the daemon's choosing, so the
route fails precisely where a device is wanted. sysfs does work: both
`/sys/class/scsi_tape/nst0/device` and `/sys/class/scsi_generic/sgN/device` are
symlinks to the same SCSI `H:C:T:L` address, so two readlinks and a comparison
pair a tape node with its sg node. A host with one drive has nothing to
disambiguate, which leaves `-device` an override for a host with two rather than
something an operator types.

Discovery is not access — SG_IO still wants the privileges — and none of it
touches §8.1's open question of whether the drive answers LOG SENSE at all while
ltfs holds the mount.

### 10.2 The plan and the catalog are one pair

`write` took `-plan` and `-catalog` as independent arguments and never compared
them. They were never independent: **a plan is a delta against one catalog**,
and its volume IDs, its generation and its deletion list mean nothing beside a
different one.

Crossing them was silent and unrecoverable. The plan's volume ID was stamped
onto a blank cartridge next to the *other* archive's ID, the other catalog
recorded files on a volume belonging to neither archive, and `verify` then
reported that cartridge **Clean** — because after the fact the arrangement is
entirely self-consistent. `volumes` listed it without comment. There is no
later check that could have found it, which is why this one has to run before
the first object is placed.

So the plan records what it was computed against, in three optional fields:

| Field | Purpose |
|---|---|
| `catalog` | absolute path — the default for `-catalog`, so the pair is named once |
| `archive` | the archive that catalog belonged to; empty until the archive has been written to once |
| `id` | this plan, distinct from every other |

**Not a fingerprint of the catalog file.** The obvious check — hash the catalog
the plan was built from, compare at write — is wrong here, and wrong in a way
that only shows up on the second cartridge: `write` rewrites the catalog after
every volume, so volume 2 of a plan legitimately meets a different file than
the plan was built from. What has to match is the *lineage*, which is the
archive, and the *cycle*, which is the plan.

**Two checks, because one does not reach.** The archive comparison is exact but
blind during an archive's first cycle: a plan made against an empty catalog has
no archive ID to carry, and that is precisely when two fresh archives look
alike. So `GenerationInfo` gains a `plan` field — whichever plan wrote a
generation's first volume is named in the timeline, and the rest of that cycle
has to be the same plan. That also catches the ordinary version of the mistake,
which is not two archives at all but two plan files for one cycle: re-planning
only advances the generation once a volume of the current one has landed.

**An absent field is a check that does not run, never one that fails.** All
three are optional so an older plan file still works, and both sides of the
cycle comparison can be missing. Getting this wrong strands a cycle halfway
through on nothing but a version difference — which the first draft did, caught
by `TestPlanWithoutIdentityIsNotRefused`. The same rule is what keeps an
interrupted *first* write resumable: the archive ID is persisted before the
first object, so that catalog names an archive while holding no records and no
timeline entry, and any check strict enough to refuse it would re-create the
orphaned-cartridge bug §11 exists to prevent.

**The plan is also a third source of archive identity**, after the medium and
before minting. `ensureArchiveID` prefers the loaded volume, because the medium
is the authority; but a catalog lost entirely, with a blank cartridge in the
drive, previously minted a second identity for an archive that already had one.
The plan remembers.

### 10.3 The workdir: the operator names no state at all

§10.2 stopped the operator from having to name the catalog *twice*. The
question it did not ask is why they were naming it at all.

**The catalog is kura's bookkeeping, not the operator's file.** Every command
took `-catalog`, and that leaked an internal artifact into the interface three
ways at once. The operator became responsible for a file they cannot usefully
read or repair. Container staging silently landed in whatever directory the
catalog happened to sit in — `StageDir: filepath.Dir(catalogPath)`, a coupling
nobody chose. And two independent path arguments could name state belonging to
two different archives, which is the failure §10.2 exists to catch.

So there is **one directory per archive**, and the commands take that:

```
/var/lib/kura/<archive-id>/       $KURA_HOME overrides the root
  catalog.jsonl.gz                the index
  plan.json                       the current cycle
  stage/                          containers, assembled before being placed
```

| | |
|---|---|
| `-workdir <dir>` | that directory, wherever it is |
| `-archive <id>` | which archive, when the root holds several |
| neither | the one archive under the root — the ordinary case |

**Resolution is the search an operator would do by hand.** The named directory
if it is itself a workdir; then `<dir>/<archive-id>` if an archive was named;
then the single archive under the root. Several archives and no `-archive` is
an error rather than a choice — one host really does hold more than one, since
§8.1 prefers a second archive to erasure coding, and a rule that picked would
eventually pick wrong on a command that writes 2.5 TB.

**`-archive` is an assertion, not a lookup.** It mirrors `-volume` for `verify`
and `restore`: a script that meant a particular archive is stopped rather than
pointed at whichever one the path turned out to hold. Making it a lookup would
need a registry, and could not name a brand-new archive at all.

**The archive ID is now minted by `plan`, not by the first write.** This is a
reversal, and it is forced: a workdir named by its archive needs that name
before `plan` can place a single file, and `plan` runs before any cartridge
exists. It costs nothing that was being relied on. The medium stays the
authority — `ensureArchiveID` still prefers what the loaded volume says, and
takes the plan's ID only when the catalog has none, ahead of minting a second
identity for an archive that already has one. If anything the
interrupted-first-write hazard shrinks: the ID reaches a file on disk before
any object is written rather than during the write that might be killed.

**A workdir is still a cache.** DESIGN §11 is unchanged: delete the directory
and it rebuilds from any cartridge the archive owns.

**`plan` refuses to replace a part-written cycle.** While the operator named
the plan file, overwriting one was their own doing; now that kura owns it, a
second `kura plan` would destroy the only copy of a cycle that has cartridges
on the shelf and volumes still to write. No bytes are lost — files that were
never written are still new, and the next cycle picks them up — but the volumes
already written are stranded in a plan nothing can finish. *Part* written is the
whole condition: a plan nothing has been written from is free to replace, which
is how an operator reconsiders before committing media, and a plan every volume
of which is written is a finished cycle with nothing left to strand. `-force`
is the way past it.

---

## 11. Startup reconciliation

The medium is the authority; the local database is a cache.

| State | Meaning | Action |
|---|---|---|
| Match | Same archive, same generation | Proceed |
| Volume ahead | Written by another machine, or after the DB was copied | Adopt the volume's catalog |
| DB ahead | A previous write did not complete, or an older volume was inserted | **Stop.** These need opposite responses |
| Divergent | Both advanced from a common ancestor | **Stop.** Requires explicit merge |
| Foreign | Different archive UUID | Refuse, and name the archive it belongs to |
| Blank | No header | Offer to initialise, with confirmation |

### 11.1 Pausing a write, and telling a pause from a crash

A volume takes hours. The operator wants the host back, or the building loses
power on a schedule, so `write` has to be stoppable — and the only stop worth
having is one that survives the machine being turned off, because "leave it
running but idle" is precisely the `tar -M` failure this tool exists to avoid.

So **a pause is not a process state.** It is a clean stop, and the resume is the
same command run again. Most of that already exists: `existing()` indexes what
the medium holds, and a re-run recognises its own earlier objects rather than
failing on the first name it already used.

What is new is the distinction. A crash and a pause leave the same medium
behind, and they have earned opposite amounts of trust.

**Stop after the object, never inside it.** The check sits between objects —
including between the staging and the placing of a container — so whatever was
in flight is finished and whole. Mid-object cancellation was considered for the
sake of a faster stop and is wrong here: an object name is a pure function of
its path, `Put` refuses to overwrite, and the medium never reclaims, so a
half-written object that had to be abandoned would burn its own name. The price
is latency. The bound is one object, which on the real archive means the largest
single file — 513 of them exceed 10 GiB — or `-tar-max` for a container. At
LTO-6 speeds a 100 GiB file is minutes of apparent deafness, so the request is
acknowledged immediately and names what it is waiting for. A second request
stops at once and accepts being a crash.

**Sync, then the marker.** The ordering is the whole guarantee. `Put` promises
atomicity and `Sync` promises durability (DESIGN §8.1a), and a host that powers
off takes the page cache with it, so a pause flushes the volume *before* it
records that it paused. The marker therefore cannot exist unless the bytes it
describes are durable, which makes its presence exactly co-extensive with the
thing a resume needs to believe. A crash has neither half.

On tape that guarantee is only as good as `Volume.Sync`, which is not yet built
and which §10.1 expects to be the `ltfs.sync` extended attribute — an assumption,
not a measurement. If it turns out that LTFS only lands its index at unmount,
then a pause on tape is durable at unmount and not before, and the honest shape
of this changes: the marker would have to be written after the volume is
released rather than after the flush, and "switch the host off" would become
"eject first, then switch the host off". The file backend has no such doubt; its
`Sync` fsyncs every object and then every directory that names one.

**The marker is local, and the medium carries no trace of it.** It lives in the
workdir beside the plan and the catalog. Three reasons, in order of weight. The
medium has no room for it: nothing mutable goes at the root, overwriting a name
is an error, so a marker on the medium would have to be a fresh object under
`generations/gNNNN/` per pause, stranding the previous one on media that cannot
reclaim. "Was this stop deliberate?" is a fact about the process that stopped,
and the process's state is local by construction — the medium stays the
authority on what *exists*, which `existing()` already reads off it. And a
marker that can go missing fails in the safe direction: a lost workdir, or a
resume on another host, falls back to the crash path, which is correct rather
than broken. **The on-media format does not change for any of this.**

**A resumed pause is trusted. A resumed crash is not.** The tempting answer is
to re-read the medium either way and trust nothing, and it is wrong three times
over.

It is redundant. The verification wanted here already exists and is already
mandatory: `verify` reads the whole volume back against its digests at the end
of the cycle. A re-read at resume is a third pass over the same bytes — write,
resume, verify — and catches nothing the third pass does not.

It is weaker than it looks. A re-read compares the medium against itself, so it
cannot see that a source file was edited at the same size while the host was
off, which is the first hazard `resume-trust.nix` names. A marker holds each
source's size, ctime and inode as of the read, so the resume re-stats and
catches exactly that, for a `stat` rather than a read.

And it is not free of consequences. An object name is a pure function of its
path, so a resume that decides it cannot vouch for an object **cannot replace
it** — there is no second name to write it under on this volume. All it can do
is report the file, leave it out of the catalog, and let the next cycle put it
on a different cartridge, which is what `resume-trust.nix` demands. The object
stays where it is, indexed by nothing, on media that never reclaims. That cost
is forced for a crash, whose records died with the process. It is not forced for
a pause, which saved them.

So: a pause saves the records it computed, and a resume adopts them after
checking that the world has not moved under it.

| Gate | Why |
|---|---|
| archive, plan ID, generation, volume ID all match | A marker is bound to one cycle for the same reason a plan is (§10.2). It is a third thing that can be crossed with a plan and a catalog, and the workdir is what keeps it uncrossable by accident |
| the medium still holds each object at the recorded length | The marker describes the medium; if they disagree the marker is stale, whatever the reason |
| each source still stats to the recorded size, ctime and inode | The host may have been off for a week. A changed source means the record would describe bytes that are no longer what that path holds |

A gate that fails does not invalidate the marker — the other objects are still
adopted. But it does not drop that object to the crash path either, and the
reason is the same one that makes the crash path expensive. The object's name is
a pure function of its path, `Put` will not overwrite it, and the medium never
reclaims, so there is no second name on this volume to write a corrected copy
under. Reading it back would only compare the medium with itself. So a refused
object is **reported and left out of the catalog**, and the next cycle writes
that file to a different cartridge — which is exactly what `resume-trust.nix`
asks for. The bytes stay where they are, indexed by nothing, which is the price
of the refusal and is why the gates are cheap `stat`s rather than guesses.

The marker is gzipped JSONL, a header line then one line per object, for the
reason the catalog is. It holds an entry per file, and the archive's largest
volume holds 539,275 of them: measured against that volume's real paths, one
indented JSON object costs 1,247 bytes an entry — 673 MB, allocated whole and
fsynced at every pause — where the streamed gzipped form costs 38, or 20.5 MB.
Most of the difference is gzip absorbing the paths, which appear twice per entry
because a source stamp and a record both name the file; that duplication is
therefore left alone rather than engineered away.

A marker vouches for a prefix and says plainly what it could not vouch for.
`-verify-resumed` re-reads the adopted objects and checks them against the
digests the marker holds, for an operator who would rather pay twice; it is off
by default because the mandatory verify is the same check, done once.

A marker that cannot be read at all — a format from a newer build, a file whose
tail a full disk took — is **ignored with a warning, never fatal.** It is an
optimisation over a path that still works, so a write must not be stopped by one.
That is also why the header carries the number of objects in the stream: without
it a truncated marker simply vouches for fewer objects, which looks exactly like a
shorter pause, and a file that lost its tail is a file whose middle is in doubt.

**Three things a pause must not do**, each of which was a real trap rather than a
hypothetical one.

It must not write the catalog onto the medium. The copies are named
`generations/gNNNN/catalog.NNN.jsonl.gz`, `writeCatalog` skips a name already
present, and overwriting is an error — so a mid-volume copy would be the one the
volume keeps, and the cartridge would end up indexing only what preceded the
pause. That is the generation-numbering bug of §8.1a arriving by a different
road. A paused volume carries no on-media index until it finishes, exactly as a
crashed one does.

It must not write the catalog locally either. `alreadyWritten` decides which
volume `write` picks next by looking for records of this generation on it, so
committing a paused volume's records would make the next run skip the cartridge
it is meant to be finishing. The records belong in the marker, which is what the
marker is: the writer's uncommitted `Result.Records`.

And the marker counts as a part-written cycle. `plan` refuses to replace a plan
whose cycle is unfinished (§10.3), judged by whether any volume holds records of
that generation — which a paused *first* volume does not. Without the marker in
that test, a pause followed by `kura plan` would quietly abandon a cartridge
holding objects that nothing indexes and no plan can finish.

`-force` is still the way past that, and past it the marker is retired rather
than left lying about — otherwise every write for the rest of the archive's life
would report a pause nothing can resume. Retiring it is also the moment to name
the cartridge, while the operator can still go and look at it: its objects are
there, indexed by nothing, and the space does not come back. The files are safe,
because nothing recorded them as stored and the new plan writes them again.

Two smaller things fall out of the records living in the marker rather than the
catalog, and both were found by building it. The files a run could not read are
in the Result and nowhere else, so the marker has to carry them too or pausing a
volume silently loses the one list that tells the operator what the next cycle
owes a write. And the volume's inventory row counts only what the run that wrote
it placed, so a cartridge finished from a marker that vouched for all but one of
its objects was listed as holding that one object — on a real volume, most of a
terabyte of under-reporting in the table an operator scrubs from. Objects carried
rather than written now count towards the row and not towards the throughput
figure, which are two different questions about the same bytes.

**`kura pause`.** A signal is the obvious trigger and not a sufficient one: the
write that most needs pausing is the one running for four hours under a unit
file or behind an SSH session that has already dropped, where there is no
terminal to press a key in. So there are two triggers and one predicate behind
them. `SIGINT`, `SIGTERM` and `SIGHUP` all pause — `SIGHUP` included precisely
because a dropped session is an unexpected halt today and a clean stop is
strictly better than dying dirty — and `kura pause` sets a flag in the workdir
that the running write notices. Neither is a mode: a `write` that was never
asked to stop behaves as it always did.

A pause exits **75**. Not 0, because a script that reads success would advance
to the next cartridge and leave this one half written; not 1, because a pause is
not a failure and should not page anybody. `EX_TEMPFAIL` is the meaning wanted —
try again later — and it is the same code whether a signal or the flag asked for
the stop, which a signal-derived 130 could not be.

`verify` and `restore` stop on the same signals and need none of this apparatus.
Neither has anything to commit: `verify` only reads, and `restore` writes each
file through a temporary and a rename, so a stop leaves a correct subset of the
tree and a re-run simply does it again. They cancel their context and say where
they got to. `plan` is deliberately left out — a cold scan is 35 minutes and
persists nothing mid-flight, so a clean stop there can only lose the scan; a
scan journal is a different feature and has to argue for itself separately.

---

## 12. Implementation

**Go.** The planner is Go, tested, and working; the workload is I/O-bound rather
than CPU-bound, so AES-NI clears 160 MB/s with room to spare and Rust's
advantage does not appear. Static single-file distribution is Go's default,
which matters for a tool people copy onto a backup host.

Both languages handle the raw SCSI this needs — `SPOUT`/`SPIN` for keys,
`LOG SENSE` for TapeAlert, MAM reads — through `ioctl(SG_IO)`.

What would change the answer: wanting the cryptographic core to carry Rust's
guarantees, or an existing Rust codebase to sit alongside.
