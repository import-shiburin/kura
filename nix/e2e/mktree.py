#!/usr/bin/env python3
"""Deterministic source trees for the e2e suite.

Two properties matter and neither is negotiable.

Deterministic: the same seed produces byte-identical files, so a digest
recorded in one scenario means something in the next one, and a failure
reproduces instead of being chased.

Shaped like the real archive: the bucket sizes straddle the 1 MB tar
threshold, because that threshold is the decision the whole on-media layout
rests on (CLAUDE.md, "File size distribution"). A tree of uniformly sized
files would exercise one side of it and report success.

The absolute sizes are scaled down — a VM cartridge is megabytes, not
terabytes — but the *shape* is kept: many files holding almost no bytes, and a
few files holding almost all of them.
"""

import argparse
import hashlib
import json
import os
import random
import sys

# Byte ranges per bucket. The names below and above 1 MB are what the scenarios
# reason about, so they are worth reading as a pair: tiny+small go into tar
# containers, medium+large are addressed individually.
BUCKETS = {
    "tiny": (1, 1023),  # < 1 KiB      — 17% of real files, 0.0% of real bytes
    "small": (1024, 1048575),  # 1 KiB – 1 MiB — 70% of files, 0.4% of bytes
    "medium": (1048576, 8 * 1024 * 1024),  # above the tar threshold
    "large": (32 * 1024 * 1024, 64 * 1024 * 1024),  # forces splits and volume turnover
}

CHUNK = 1 << 20


def parse_spec(spec):
    """"tiny=200,small=50" -> {"tiny": 200, "small": 50}."""
    out = {}
    for part in spec.split(","):
        part = part.strip()
        if not part:
            continue
        name, _, count = part.partition("=")
        if name not in BUCKETS:
            sys.exit(f"mktree: unknown bucket {name!r}; have {', '.join(BUCKETS)}")
        out[name] = int(count)
    return out


def write_file(path, size, rng):
    """Write `size` deterministic bytes.

    Chunked because a large bucket file is tens of megabytes and materialising
    that as one bytes object costs more memory than the test VM has to spare.
    """
    h = hashlib.sha256()
    with open(path, "wb") as f:
        left = size
        while left:
            n = min(left, CHUNK)
            buf = rng.randbytes(n)
            f.write(buf)
            h.update(buf)
            left -= n
    return h.hexdigest()


def layout(rng, index):
    """A nested path, so grouping and -max-group have a real tree to work on."""
    return os.path.join(f"d{index // 64:03d}", f"s{index // 8 % 8:02d}")


def cmd_gen(args):
    root = args.root
    rng = random.Random(args.seed)
    spec = parse_spec(args.spec)

    os.makedirs(root, exist_ok=True)
    manifest = []
    total = 0
    index = 0

    for bucket in sorted(spec):
        lo, hi = BUCKETS[bucket]
        for i in range(spec[bucket]):
            size = rng.randint(lo, hi)
            rel = os.path.join(layout(rng, index), f"{bucket}-{i:05d}.bin")
            path = os.path.join(root, rel)
            os.makedirs(os.path.dirname(path), exist_ok=True)
            digest = write_file(path, size, rng)
            manifest.append({"path": rel, "size": size, "sha256": digest})
            total += size
            index += 1

    if args.max_bytes and total > args.max_bytes:
        sys.exit(f"mktree: generated {total} bytes, over the --max-bytes cap of {args.max_bytes}")

    if args.manifest:
        with open(args.manifest, "w") as f:
            json.dump(manifest, f, indent=1, sort_keys=True)

    print(f"files={len(manifest)} bytes={total}")


def cmd_mutate(args):
    """Change the tree the way a real one changes between cycles.

    Modify, delete and add, rather than only add: a backup that is only ever
    tested against growth never exercises deletion records, and deletion is
    where change detection has historically gone wrong here (see
    TestDeletionAfterRewriteStaysDeleted).
    """
    root = args.root
    rng = random.Random(args.seed)

    existing = []
    for dirpath, _, names in os.walk(root):
        for n in sorted(names):
            existing.append(os.path.join(dirpath, n))
    existing.sort()

    if not existing and (args.modify or args.delete):
        sys.exit("mktree: nothing to mutate")

    picked = rng.sample(existing, min(args.modify + args.delete, len(existing)))
    modified, deleted, added = [], [], []

    for path in picked[: args.modify]:
        # Rewrite in place at the same length. Same size, new content is the
        # case that catches a change detector trusting size alone.
        st = os.stat(path)
        write_file(path, st.st_size, rng)
        if args.preserve_mtime:
            # Same size, same mtime, different bytes: the only remaining
            # signal is ctime, which is why DESIGN §6 refuses to trust mtime
            # on its own. Restoring mtime at nanosecond precision leaves ctime
            # as the sole difference, on a real filesystem rather than a
            # simulated one.
            os.utime(path, ns=(st.st_atime_ns, st.st_mtime_ns))
        modified.append(os.path.relpath(path, root))

    for path in picked[args.modify : args.modify + args.delete]:
        os.remove(path)
        deleted.append(os.path.relpath(path, root))

    for i in range(args.add):
        lo, hi = BUCKETS[args.add_bucket]
        rel = os.path.join("added", f"{args.seed}-{i:04d}.bin")
        path = os.path.join(root, rel)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        write_file(path, rng.randint(lo, hi), rng)
        added.append(rel)

    print(json.dumps({"modified": modified, "deleted": deleted, "added": added}, sort_keys=True))


def cmd_manifest(args):
    """Digest a tree, so two trees can be compared by content alone."""
    out = []
    for dirpath, _, names in os.walk(args.root):
        for n in sorted(names):
            path = os.path.join(dirpath, n)
            h = hashlib.sha256()
            with open(path, "rb") as f:
                while True:
                    buf = f.read(CHUNK)
                    if not buf:
                        break
                    h.update(buf)
            out.append(
                {
                    "path": os.path.relpath(path, args.root),
                    "size": os.path.getsize(path),
                    "sha256": h.hexdigest(),
                }
            )
    out.sort(key=lambda e: e["path"])
    json.dump(out, sys.stdout, indent=1, sort_keys=True)
    sys.stdout.write("\n")


def cmd_corrupt(args):
    """Flip one byte inside a file on the medium.

    The point is not the flip; it is that verify must attribute the damage to
    the one file it destroyed rather than condemning the whole container.
    """
    with open(args.path, "r+b") as f:
        f.seek(args.offset)
        b = f.read(1)
        if not b:
            sys.exit(f"mktree: offset {args.offset} is past the end of {args.path}")
        f.seek(args.offset)
        f.write(bytes([b[0] ^ 0xFF]))
    print(f"flipped byte {args.offset} of {args.path}")


def cmd_corrupt_member(args):
    """Flip a byte inside one member's data in a tar container.

    Exact rather than "somewhere in the file": a random offset in a tar lands
    in a header or in padding often enough that the test would sometimes prove
    a different thing than it claims. tarfile knows where each member's bytes
    start, so the damage can be aimed at one file and the assertion can name
    it.
    """
    import tarfile

    with tarfile.open(args.path) as tf:
        members = [m for m in tf.getmembers() if m.isfile() and m.size > 16]
        if not members:
            sys.exit(f"mktree: {args.path} has no member big enough to damage")
        target = max(members, key=lambda m: m.size)
        offset = target.offset_data + target.size // 2

    with open(args.path, "r+b") as f:
        f.seek(offset)
        b = f.read(1)
        f.seek(offset)
        f.write(bytes([b[0] ^ 0xFF]))

    print(target.name)


def cmd_fill(args):
    """Consume free space on a mounted cartridge, leaving a known margin.

    This is how a scenario arranges "the medium is nearly full" without first
    writing a realistic amount of data into it.
    """
    st = os.statvfs(args.mount)
    free = st.f_bavail * st.f_frsize
    target = max(free - args.leave, 0)
    path = os.path.join(args.mount, ".ballast")
    written = 0
    with open(path, "wb") as f:
        buf = b"\0" * CHUNK
        while written + CHUNK <= target:
            f.write(buf)
            written += CHUNK
        f.write(b"\0" * (target - written))
        written = target
        f.flush()
        os.fsync(f.fileno())
    print(f"ballast={written} left={args.leave}")


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    sub = ap.add_subparsers(dest="cmd", required=True)

    g = sub.add_parser("gen", help="create a tree")
    g.add_argument("root")
    g.add_argument("--seed", type=int, default=1)
    g.add_argument("--spec", default="tiny=120,small=60,medium=6,large=1")
    g.add_argument("--manifest")
    g.add_argument("--max-bytes", type=int, default=0)
    g.set_defaults(func=cmd_gen)

    m = sub.add_parser("mutate", help="change a tree between cycles")
    m.add_argument("root")
    m.add_argument("--seed", type=int, default=2)
    m.add_argument("--modify", type=int, default=0)
    m.add_argument("--delete", type=int, default=0)
    m.add_argument("--add", type=int, default=0)
    m.add_argument("--add-bucket", default="small", choices=sorted(BUCKETS))
    m.add_argument(
        "--preserve-mtime",
        action="store_true",
        help="rewrite content but restore mtime, leaving ctime as the only signal",
    )
    m.set_defaults(func=cmd_mutate)

    n = sub.add_parser("manifest", help="digest a tree")
    n.add_argument("root")
    n.set_defaults(func=cmd_manifest)

    c = sub.add_parser("corrupt", help="flip one byte in a file")
    c.add_argument("path")
    c.add_argument("--offset", type=int, required=True)
    c.set_defaults(func=cmd_corrupt)

    cm = sub.add_parser("corrupt-member", help="flip a byte inside one tar member")
    cm.add_argument("path")
    cm.set_defaults(func=cmd_corrupt_member)

    f = sub.add_parser("fill", help="eat free space on a mount")
    f.add_argument("mount")
    f.add_argument("--leave", type=int, required=True)
    f.set_defaults(func=cmd_fill)

    args = ap.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
