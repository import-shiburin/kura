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

# One workdir per archive, named by the operator rather than by the archive ID,
# because a scenario has to name the same archive again before any ID exists.
# The archive-ID form under KURA_HOME is what an operator gets; it is exercised
# in workdir.nix rather than assumed by every scenario here.
WORKDIR = "/var/lib/kura/main"
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


def load(machine, slot):
    """Insert the cartridge from `slot` into the drive, and return the mount.

    Only the slot is named, because that is all the operator controls. Which
    volume the cartridge turns out to be is written on it, and the mount point
    is the same path every time — a drive is one path, and the tool has to
    identify the medium without help from it.
    """
    return machine.succeed(f"cartridge load {slot}").strip()


def unload(machine):
    machine.succeed("cartridge unload")


def free_bytes(machine):
    """Free space on whatever cartridge is currently loaded."""
    return int(machine.succeed("cartridge free").strip())


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


def catalog_header(machine, path=None):
    """The catalog's header line, decompressed in the guest.

    Read with python rather than `zcat | head -1`, which fails under the
    driver's `set -o pipefail` when head closes the pipe early.
    """
    path = path or catalog_path()
    prog = (
        "import gzip,json,sys;"
        "f=gzip.open(sys.argv[1]) if sys.argv[1].endswith('.gz') else open(sys.argv[1],'rb');"
        "print(f.readline().decode())"
    )
    return json.loads(machine.succeed(f"python3 -c {shlex.quote(prog)} {shlex.quote(path)}"))


def catalog_path(workdir=WORKDIR):
    """Where a workdir keeps its catalog.

    Scenarios read it directly to assert on the header; nothing passes it to
    kura, which is the entire point of the workdir — see DESIGN §10.3.
    """
    return os.path.join(workdir, "catalog.jsonl.gz")


def plan_path(workdir=WORKDIR):
    """Where kura plan leaves the plan kura write picks up."""
    return os.path.join(workdir, "plan.json")


def plan_volumes(machine, path=None):
    """The volume IDs a plan lays out, in order."""
    p = read_json(machine, path or plan_path())
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


def write_volume(machine, slot, *extra, workdir=WORKDIR, check=True):
    """The operator's inner loop: load a cartridge, write one volume, eject.

    Each write is its own process that exits before the next cartridge goes in.
    That is the contract the whole tool is built around — no process is held
    open across a swap — so the harness performs it literally rather than
    calling write twice against one mount.

    No volume ID: which volume this becomes is the plan's decision for a blank
    cartridge, and the medium's own answer for one that has been written. Pass
    `-volume X` through `extra` to write a particular volume of the plan.
    """
    mount = load(machine, slot)
    out = kura(machine, "write", "-workdir", workdir, "-mount", mount, *extra, check=check)
    unload(machine)
    return out


def verify_volume(machine, slot, *extra, workdir=WORKDIR, check=True):
    mount = load(machine, slot)
    out = kura(machine, "verify", "-workdir", workdir, "-mount", mount, *extra, check=check)
    unload(machine)
    return out


def restore_volume(machine, slot, into, *extra, workdir=WORKDIR, check=True):
    mount = load(machine, slot)
    machine.succeed(f"mkdir -p {shlex.quote(into)}")
    out = kura(machine, "restore", "-workdir", workdir, "-mount", mount, "-into", into, *extra, check=check)
    unload(machine)
    return out


def write_all(machine, slots, *extra):
    """Write every volume of a plan, one cartridge at a time.

    The plan's volume order and the slot order are the operator's to line up;
    each write takes the first volume not yet written, so a blank cartridge in
    each slot produces the plan's volumes in sequence.
    """
    return [write_volume(machine, slot, *extra) for slot in slots]


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
