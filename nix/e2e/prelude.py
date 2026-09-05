# Helpers shared by every e2e scenario.
#
# Prepended to each scenario's testScript, so a scenario file reads as the
# operator's story — plan, load a cartridge, write, swap, verify — and none of
# the plumbing. Anything that appears in three scenarios belongs here.
#
# One thing about the driver is worth knowing before writing a shell command
# here: every command it runs is wrapped in `set -euo pipefail`. So `find a b |
# wc -l` fails outright when b does not exist yet rather than reporting what it
# found, and a loop whose body contains a command that may fail ends the whole
# command instead of the iteration. Guard with `|| true`, or use a helper below
# that already does.

import json
import os
import re
import shlex
from datetime import timedelta

STORAGE = "/srv/storage"
CATALOG = "/var/lib/kura/catalog.jsonl.gz"
PLAN = "/var/lib/kura/plan.json"
COVERAGE_DIR = "/var/lib/kura-coverage"


def kura(machine, *args, check=True):
    """Run kura and return its combined output.

    check=False is for the paths that are supposed to fail. Those matter more
    than the happy ones here: refusing the wrong cartridge, refusing a foreign
    volume, refusing to overwrite an object name. A harness that can only
    express success cannot test any of them.
    """
    cmd = "kura " + " ".join(shlex.quote(str(a)) for a in args) + " 2>&1"
    if check:
        return machine.succeed(cmd)
    status, out = machine.execute(cmd)
    assert status != 0, f"expected `kura {' '.join(str(a) for a in args)}` to fail, but it succeeded:\n{out}"
    return out


def kura_status(machine, *args):
    """Run kura and return (exit status, output) without asserting either way."""
    cmd = "kura " + " ".join(shlex.quote(str(a)) for a in args) + " 2>&1"
    return machine.execute(cmd)


def mktree(machine, *args):
    return machine.succeed("mktree " + " ".join(shlex.quote(str(a)) for a in args))


def make_source(machine, spec="tiny=120,small=60,medium=6,large=1", seed=1, root=STORAGE):
    """Create the tree to be archived, and return (file count, total bytes)."""
    out = mktree(machine, "gen", root, "--seed", seed, "--spec", spec)
    fields = dict(kv.split("=") for kv in out.split())
    return int(fields["files"]), int(fields["bytes"])


def blank(machine, slot):
    """Format a cartridge, as if unwrapping a new one."""
    machine.succeed(f"cartridge blank {slot}")


def load(machine, slot, volume_id):
    """Insert the cartridge in `slot` and present it as `volume_id`.

    Slot and volume ID are separate arguments on purpose. They are the same
    thing only when the operator loads the right cartridge, and the scenarios
    that matter are the ones where they are not.
    """
    return machine.succeed(f"cartridge load {slot} {volume_id}").strip()


def unload(machine):
    machine.succeed("cartridge unload")


def free_bytes(machine, volume_id):
    return int(machine.succeed(f"cartridge free {volume_id}").strip())


def restored_path(into, source=STORAGE):
    """Where a restore actually lands.

    The catalog stores absolute paths and a restore rebases them under -into,
    so a tree archived from /srv/storage reappears at <into>/srv/storage. Every
    scenario that compares trees needs this, and getting it wrong compares an
    empty directory against itself and passes.
    """
    return os.path.join(into, source.lstrip("/"))


def count_files(machine, *dirs):
    """How many files are under these directories, treating a missing one as 0.

    A scenario watching a write in progress asks this before the writer has
    created `data/` or `containers/`, and under the driver's `set -o pipefail`
    a bare `find` over a missing directory fails the pipeline rather than
    counting what does exist.
    """
    paths = " ".join(shlex.quote(d) for d in dirs)
    return int(machine.succeed(f"find {paths} -type f 2>/dev/null | wc -l || true").strip())


def wait_for(machine, cmd, seconds=120):
    """wait_until_succeeds with a duration the driver does not deprecate.

    Wrapped rather than called directly so timedelta is imported in one place:
    the driver lints the whole concatenated script, and an import used by only
    some scenarios would fail the others.
    """
    machine.wait_until_succeeds(cmd, timeout=timedelta(seconds=seconds))


def wait_until_gone(machine, cmd, seconds=60):
    machine.wait_until_fails(cmd, timeout=timedelta(seconds=seconds))


def kill_once_started(machine, pidfile, watch_dirs, threshold=2):
    """Kill a running kura as soon as it has placed `threshold` objects.

    See the killwhen script in base.nix for why this cannot be done from the
    driver side. Returns what it killed and how far the run had got, so a
    scenario can log it.
    """
    dirs = " ".join(shlex.quote(d) for d in watch_dirs)
    return machine.succeed(f"killwhen {shlex.quote(pidfile)} {threshold} {dirs}").strip()


def start_background(machine, cmd, pidfile, logfile):
    """Start a command in the guest and record its pid.

    kura is a wrapper that execs the real binary, so the pid recorded here is
    the pid that has to be signalled.
    """
    machine.succeed(f"({cmd} >{logfile} 2>&1 & echo $! >{pidfile})")


def read_json(machine, path):
    return json.loads(machine.succeed(f"cat {shlex.quote(path)}"))


def catalog_header(machine, path=CATALOG):
    """The catalog's header line, decompressed in the guest.

    Read with python rather than `zcat | head -1`, which fails under the
    driver's `set -o pipefail` when head closes the pipe early.
    """
    prog = (
        "import gzip,json,sys;"
        "f=gzip.open(sys.argv[1]) if sys.argv[1].endswith('.gz') else open(sys.argv[1],'rb');"
        "print(f.readline().decode())"
    )
    return json.loads(machine.succeed(f"python3 -c {shlex.quote(prog)} {shlex.quote(path)}"))


def plan_volumes(machine, path=PLAN):
    """The volume IDs a plan lays out, in order."""
    p = read_json(machine, path)
    return [v["id"] for v in p.get("volumes", [])]


def manifest(machine, root):
    return json.loads(machine.succeed(f"mktree manifest {shlex.quote(root)}"))


def assert_trees_identical(machine, source, restored):
    """Byte-for-byte equality, reported as a diff rather than as a boolean.

    `diff -r` names the first file that differs, which is the difference
    between a failing test and a debuggable one.
    """
    machine.succeed(f"diff -r --no-dereference {shlex.quote(source)} {shlex.quote(restored)}")


def assert_contains(haystack, needle, what="output"):
    assert needle in haystack, f"{what} did not contain {needle!r}:\n{haystack}"


def state_file_count(report):
    """The file count out of an `at-gen` summary.

    A helper rather than an inline regex because the driver type-checks the
    test script, and `re.search(...).group(1)` is rejected outright: search
    returns None when it does not match. Handling that here also turns a
    changed report format into a readable failure instead of an AttributeError.
    """
    m = re.search(r"^\s+(\d+) files,", report, re.M)
    assert m is not None, f"no file count in the at-gen summary:\n{report}"
    return int(m.group(1))


def delta_counts(report):
    """The DELTA vs CATALOG table from a plan report, as numbers.

    Matched by regex rather than by exact string: the report is laid out with
    a tabwriter, so the spacing between a label and its count depends on the
    widest row and changes as the numbers do.
    """
    counts = {}
    for label in ("unchanged", "new", "modified", "already written", "deleted"):
        m = re.search(rf"^\s+{re.escape(label)}\s+(\d+)\s", report, re.M)
        if m:
            counts[label] = int(m.group(1))
    return counts


def write_volume(machine, slot, volume_id, *extra):
    """The operator's inner loop: load a cartridge, write one volume, eject.

    Each write is its own process that exits before the next cartridge goes in.
    That is the contract the whole tool is built around — no process is held
    open across a swap — so the harness performs it literally rather than
    calling write twice against one mount.
    """
    mount = load(machine, slot, volume_id)
    out = kura(machine, "write", "-plan", PLAN, "-catalog", CATALOG, "-drive", mount, *extra)
    unload(machine)
    return out


def verify_volume(machine, slot, volume_id, *extra, check=True):
    mount = load(machine, slot, volume_id)
    out = kura(machine, "verify", "-catalog", CATALOG, "-drive", mount, *extra, check=check)
    unload(machine)
    return out


def restore_volume(machine, slot, volume_id, into, *extra, check=True):
    mount = load(machine, slot, volume_id)
    machine.succeed(f"mkdir -p {shlex.quote(into)}")
    out = kura(machine, "restore", "-catalog", CATALOG, "-drive", mount, "-into", into, *extra, check=check)
    unload(machine)
    return out


def write_all(machine, slots, volume_ids, *extra):
    """Write every volume of a plan, one cartridge at a time."""
    outs = []
    for slot, vid in zip(slots, volume_ids):
        outs.append(write_volume(machine, slot, vid, *extra))
    return outs


def collect_coverage(machine):
    """Pull the instrumented binary's counters out of the VM.

    Appended to every scenario by the harness. Each kura invocation leaves one
    counter file behind — Go flushes on os.Exit as well as on a normal return,
    which is what makes the argument-error paths reachable at all — and the
    whole directory merges with unit coverage on the host.

    Never fatal. A scenario that proved something and then failed to hand over
    its counters should report the thing it proved, not a coverage error.
    """
    if not machine.booted:
        machine.start()
        machine.wait_for_unit("multi-user.target")

    status, out = machine.execute(f"ls -1 {COVERAGE_DIR} | wc -l")
    count = out.strip() if status == 0 else "?"
    machine.log(f"coverage: {count} counter files in {COVERAGE_DIR}")

    try:
        machine.copy_from_vm(COVERAGE_DIR, "")
    except Exception as e:  # noqa: BLE001 - reporting beats failing here
        machine.log(f"coverage: could not collect ({e})")
