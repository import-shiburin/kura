# A write that is asked to stop, and stops cleanly.
#
# interrupted-write covers the two ways a write dies: SIGKILL takes the process
# and leaves the medium, a power cut takes the page cache with it. Both are
# crashes, and a resume can do nothing but read the medium back to find out what
# is on it. This is the third case, and it is the only one with a promise
# attached — DESIGN §11.1. A pause finishes the object in flight, flushes the
# volume, writes down the records it computed, and exits 75; the resume adopts
# those records after re-stat'ing their sources, without reading a byte back.
#
# Every half of that promise is about a process that ends and a machine that
# goes away: the exit status a wrapper script reads, a cartridge that can be
# ejected, a host that can be switched off, and a workdir that outlives all
# three. None of it is reachable in process, which is why it is here.
#
# Five things are load-bearing, each one a trap §11.1 names explicitly:
#
#   * A pause commits nothing to the catalog — not locally, where records would
#     make alreadyWritten skip the very cartridge the next run is meant to be
#     finishing, and not onto the medium, where a mid-volume catalog copy would
#     be the one copy the cartridge keeps, leaving it indexing half a cycle.
#   * It is still a part-written cycle. A paused volume has no records anywhere,
#     so without the checkpoint in plan's unfinished-cycle test a pause followed
#     by kura plan would quietly abandon a cartridge holding objects that nothing
#     indexes and no plan can finish. The second archive below is where that is
#     pinned, because there the pause is on the *first* volume of the cycle and
#     the checkpoint is the only thing that knows the cycle started at all.
#   * The marker is consumed by the run that honours it, and a request left over
#     from an earlier run is cleared rather than obeyed. Otherwise the resume
#     would stop dead on its own first object.
#   * It survives the host being switched off. That is the whole point of a clean
#     stop over "leave it running but idle", which is the tar -M failure this
#     tool exists to avoid, so the reboot here is a deliberate poweroff rather
#     than interrupted-write's crash.
#   * A checkpoint is bound to one volume of one cycle. A pause on volume 1 has
#     to survive the operator writing volume 2, and be reported and left alone
#     rather than consumed.
#
# Two mechanics are worth knowing before reading the script.
#
# The exit status. A pause is judged by it — 75 is EX_TEMPFAIL, "try again
# later", where 0 would tell a wrapper script to advance to the next cartridge
# and leave this one half written. start_background records a pid and not a
# status, so the write runs under a shell that keeps both: the writer's own pid,
# which is what gets signalled, and the status it exited with.
#
# The freeze. A pause has to be requested while a write is in flight, and the
# driver polls on the order of a second while a whole volume here is written in
# rather less than that — the same problem killwhen exists for (base.nix). So the
# guest freezes the writer with SIGSTOP milliseconds after it has placed its
# first objects, and the request is made against a process that cannot outrun it.
# That changes nothing about what is under test: SIGSTOP is not one of the
# signals kura handles, and on SIGCONT the write stats the flag at its next
# object boundary exactly as it would have. It only makes "mid-volume" a fact
# instead of a hope. The freeze is held for two seconds, because the flag is
# stat'd at most once a second and a writer let go inside that window would
# finish the volume before looking again; the wait is spent frozen, so it is the
# implementation's throttle being waited on rather than the disk.
#
# Cartridges 1-4 are one archive: write, pause, withdraw, stale request. 5-7 are
# a second archive, where the outstanding pause is on the first volume of its
# cycle, where another volume is written while it stands, and where an operator
# who will not wait for the object in flight gets the crash they asked for.
{ lib, pkgs }:

{
  description = "a pause stops a write cleanly, survives a poweroff, and is told apart from a crash";

  cartridges = [
    128
    128
    128
    128
    128 # a second archive, for the pause that belongs to another volume
    128
    128 # and for the pause the operator refuses to wait for
  ];
  memorySize = 2048;
  diskSize = 6144;

  script = ''
    machine.start()
    machine.wait_for_unit("multi-user.target")

    RESUME = os.path.join(WORKDIR, "resume.jsonl.gz")
    REQUEST = os.path.join(WORKDIR, "PAUSE")
    SECOND = "/var/lib/kura/second"
    SECOND_RESUME = os.path.join(SECOND, "resume.jsonl.gz")

    def checkpoint_header(path):
        """What a paused write wrote down, without decompressing the objects.

        A checkpoint is a gzipped JSONL stream — one header line and then a line
        per object — so everything a scenario wants to know about a pause is in
        its first line. catalog_header is what reads that: it is named for the
        catalog, but a catalog and a checkpoint are the same shape, and it
        already handles the detail that `zcat | head -1` dies under the driver's
        pipefail when head closes the pipe.
        """
        return catalog_header(machine, path)

    def start_write(tag, mount, *extra, workdir=WORKDIR):
        """Start a write in the background, keeping its pid and its exit status.

        start_background records the pid of what it starts, which is all the
        other scenarios need: they kill their write and judge it by what the
        medium holds afterwards. A pause is judged by how it exited — 75 and
        nothing else (DESIGN §11.1) — so the write runs under a shell that
        records the writer's own pid, waits for it, and writes the status down.
        The pid has to be the writer's: kura is a wrapper that execs the real
        binary, but a shell running two commands does not exec either of them.
        """
        pid, rc = f"/tmp/{tag}.pid", f"/tmp/{tag}.rc"
        args = " ".join(shlex.quote(str(a)) for a in extra)
        inner = (
            f"kura write -workdir {shlex.quote(str(workdir))} -mount {shlex.quote(mount)} -quiet {args}"
            f" & p=$!; echo $p >{pid}; wait $p; echo $? >{rc}"
        )
        start_background(machine, f"sh -c {shlex.quote(inner)}", f"/tmp/{tag}.shell.pid", f"/tmp/{tag}.log")
        return pid, rc

    def freeze_once_started(pid_file, watch_dirs, threshold=2):
        """Stop the writer dead as soon as it has placed `threshold` objects.

        In the guest, for the reason killwhen is in the guest: the driver's poll
        is about a second and a volume of this size is written in a fraction of
        one, so a request made from the driver side would reach a run that had
        already finished the cartridge it was meant to stop half way through.
        This loop sits next to the thing it watches and reacts in milliseconds.

        Returns what it froze and how far the run had got, so a failure says
        whether the freeze was early or late.
        """
        dirs = " ".join(shlex.quote(d) for d in watch_dirs)
        # find exits non-zero over a directory the writer has not created yet,
        # which is the normal state at the start of a volume; under the driver's
        # `set -o pipefail` that would end the loop rather than the iteration.
        count = f"$(find {dirs} -type f 2>/dev/null | wc -l || true)"
        quoted = shlex.quote(pid_file)
        prog = "\n".join([
            f"while [ ! -s {quoted} ]; do sleep 0.01; done",
            f"pid=$(cat {quoted})",
            # Seeded rather than given a brace default in the test below: a
            # shell brace expansion inside a scenario has to be escaped from
            # Nix's own interpolation, and one more assignment reads better.
            "placed=0",
            'while kill -0 "$pid" 2>/dev/null; do',
            f"  placed={count}",
            f'  if [ "$placed" -ge {threshold} ]; then',
            '    kill -STOP "$pid" || exit 1',
            '    echo "froze $pid with $placed objects placed"',
            "    exit 0",
            "  fi",
            "  sleep 0.01",
            "done",
            'echo "the write was over before it could be frozen" >&2',
            "exit 1",
        ])
        return machine.succeed(f"sh -c {shlex.quote(prog)}").strip()

    def thaw(pid_file, dwell=2):
        """Let the flag's stat window pass, then let the writer go.

        A write looks at the flag file at most once a second — it is asked
        between every object, and a volume of the real archive holds six figures
        of them — so a writer let go inside that window would not look again
        until a second later, and a volume here is written in rather less than a
        second. The wait is spent frozen, which is what stops it being a race:
        no object is placed while it passes, and what is being waited for is the
        implementation's own throttle rather than the disk.
        """
        machine.succeed(f"sleep {dwell}", f'kill -CONT "$(cat {shlex.quote(pid_file)})"')

    def exit_status(rc_file):
        """What a backgrounded write exited with, once it has.

        Waits for a non-empty file rather than an existing one: the redirection
        creates it before the shell has written the digits into it.
        """
        wait_for(machine, f"test -s {shlex.quote(rc_file)}")
        return int(machine.succeed(f"cat {shlex.quote(rc_file)}").strip())

    def entry_count(index, workdir=WORKDIR):
        """How many plan entries one volume of the current plan holds."""
        p = read_json(machine, plan_path(workdir))
        return len((p["volumes"] or [])[index]["entries"])

    with subtest("a plan whose second volume has a long tail to leave behind"):
        # The same tree and capacity as interrupted-write, for the same reason:
        # objects rather than bytes are what give an interruption somewhere to
        # land. The volume that gets paused here is the second, which carries the
        # small-file tail — a few hundred entries, so a pause two objects in
        # leaves almost all of the volume still to go, and "mid-volume" is not a
        # figure of speech.
        make_source(machine, spec="tiny=200,small=120,medium=20", seed=67)
        kura(
            machine, "plan",
            "-root", STORAGE, "-workdir", WORKDIR,
            "-capacity", "48MB", "-max-group", "16MB",
        )
        volumes = plan_volumes(machine)
        assert len(volumes) >= 4, f"this scenario needs four volumes, plan has {volumes}"
        assert entry_count(1) >= 5, (
            f"{volumes[1]} holds {entry_count(1)} entries; a pause needs a volume with a tail left after it"
        )
        machine.log(f"plan: {volumes}")
        for slot in range(1, len(volumes) + 1):
            blank(machine, slot)

    with subtest("the first volume establishes the archive"):
        # Not the volume that gets paused. A paused first volume is its own case
        # and it is the second archive's, at the end of this scenario.
        write_volume(machine, 1, "-quiet")
        assert_contains(kura(machine, "volumes", "-workdir", WORKDIR), volumes[0], "inventory")

    with subtest("kura pause stops the write at an object boundary and exits 75"):
        # The flag file rather than a signal, because the write that most needs
        # pausing is the one already running under a unit file or behind an SSH
        # session that has dropped, where there is no terminal to send one from.
        mount = load(machine, 2)
        pid, rc = start_write("paused", mount, "-volume", volumes[1])
        machine.log(freeze_once_started(pid, [f"{mount}/data", f"{mount}/containers"]))

        placed = count_files(machine, f"{mount}/data", f"{mount}/containers")
        assert placed >= 2, f"the freeze landed before anything was written ({placed} objects)"

        out = kura(machine, "pause", "-workdir", WORKDIR)
        assert_contains(out, "to stop at the next object boundary", "what kura pause says")
        machine.succeed(f"test -s {REQUEST}")
        thaw(pid)

        status = exit_status(rc)
        assert status == 75, (
            f"a pause must exit 75 (EX_TEMPFAIL), not {status}: 0 would tell a wrapper script to "
            "move on to the next cartridge and leave this one half written"
        )

        report = machine.succeed("cat /tmp/paused.log")
        assert_contains(report, f"=== PAUSED {volumes[1]} ===", "the pause report")
        # The acknowledgement has to name what it is waiting for: the stop lands
        # at the next boundary, which for a large file is minutes away, and a run
        # that went quiet instead would look hung.
        assert_contains(report, "kura pause: pausing after", "the acknowledgement")
        # Sync ran before the checkpoint was written, which is what makes this
        # line true rather than hopeful. It is the one thing the operator most
        # needs to know and the one a pause could most easily be assumed not to
        # give.
        assert_contains(report, "is flushed and safe to eject, and the host can be switched off.", "the pause report")
        assert_contains(report, f"Resume with: kura write -mount {mount}", "the pause report")

        # Consumed by the run that honoured it. Left in place, the resume would
        # stop on its own first object.
        machine.fail(f"test -e {REQUEST}")

        checkpoint = checkpoint_header(RESUME)
        assert checkpoint["volume"] == volumes[1], (
            f"the checkpoint names {checkpoint['volume']}, the paused volume was {volumes[1]}"
        )
        assert checkpoint["generation"] == 1, f"the checkpoint names generation {checkpoint['generation']}"
        assert checkpoint["reason"] == "kura pause", (
            f"the checkpoint blames {checkpoint['reason']!r} rather than the flag file"
        )
        assert checkpoint.get("objects", 0) >= 1, (
            "the checkpoint vouches for nothing, so there was nothing to pause"
        )
        assert checkpoint.get("remaining", 0) > 0, (
            "nothing was left to write, so the run finished the volume and this proves nothing"
        )
        unload(machine)

    with subtest("the pause committed nothing, to the catalog or to the medium"):
        # Locally first. alreadyWritten decides which volume the next write picks
        # by looking for records of this generation, so a paused volume that had
        # been committed would make the next run skip the cartridge it is meant
        # to be finishing.
        inventory = kura(machine, "volumes", "-workdir", WORKDIR)
        assert volumes[1] not in inventory, (
            f"{volumes[1]} was recorded despite the pause:\n{inventory}"
        )

        # And on the medium. The copies are named generations/gNNNN/catalog.NNN
        # and writeCatalog skips a name already present, so a mid-volume copy
        # would be the one this cartridge keeps — an index describing only what
        # preceded the pause, which is the generation-numbering bug of §8.1a
        # arriving by another road.
        mount = load(machine, 2)
        indexes = count_files(machine, f"{mount}/generations")
        assert indexes == 0, f"a paused volume carries {indexes} catalog copies; it must carry none"
        objects = count_files(machine, f"{mount}/data", f"{mount}/containers")
        assert objects >= 2, f"only {objects} objects on the medium; the pause recorded more than it placed"
        unload(machine)

    with subtest("a paused volume leaves the cycle part written, so plan refuses to replace it"):
        # Without the checkpoint in that test the refusal here would still come,
        # off volume 1's records — which is why the second archive at the end
        # repeats this with the pause on the cycle's first volume, where the
        # checkpoint is the only evidence the cycle ever started. What this pins
        # is the other half: the paused volume is named as still to go, and the
        # plan a cartridge was written against is not replaced underneath it.
        before = read_json(machine, plan_path())["id"]
        out = kura(
            machine, "plan",
            "-root", STORAGE, "-workdir", WORKDIR,
            "-capacity", "48MB", "-max-group", "16MB",
            check=False,
        )
        assert_contains(out, "is only part written", "the refusal")
        assert_contains(out, volumes[1], "the refusal")
        after = read_json(machine, plan_path())["id"]
        assert before == after, (
            f"the refused plan replaced the plan anyway ({before} -> {after}), stranding the paused cartridge"
        )

    with subtest("the host is switched off and brought back"):
        # A deliberate poweroff rather than interrupted-write's crash, because
        # surviving one is the entire requirement: the pause exists so the
        # operator can have the machine back. The checkpoint is fsynced, and its
        # directory with it, so it is here on the other side.
        machine.shutdown()
        machine.start()
        machine.wait_for_unit("multi-user.target")

        checkpoint = checkpoint_header(RESUME)
        assert checkpoint["volume"] == volumes[1], "the checkpoint did not survive the poweroff"

        # What an operator types to find out where they were. Nothing is
        # outstanding to withdraw, and the answer is the report rather than an
        # error.
        out = kura(machine, "pause", "-clear", "-workdir", WORKDIR)
        assert_contains(out, "nothing to withdraw", "kura pause -clear with no request outstanding")
        assert_contains(out, f"{volumes[1]} is paused at generation 1", "what is paused")

    with subtest("the resume adopts what the pause vouched for, without reading it back"):
        out = write_volume(machine, 2, "-quiet")
        assert_contains(out, "came from a pause and were taken as written, without being read back", "the resume")
        # The distinction the checkpoint exists for. A crash's resume reads every
        # object already on the medium back before it can go on; a pause's does
        # not, and if anything here came through that path the checkpoint was not
        # trusted.
        assert "were already present from an interrupted run" not in out, (
            f"the resume read objects back despite the pause vouching for them:\n{out}"
        )
        # Spent: its records are in the catalog now, so keeping it would offer
        # them to a later run a second time.
        machine.fail(f"test -e {RESUME}")
        assert_contains(kura(machine, "volumes", "-workdir", WORKDIR), volumes[1], "inventory after the resume")
        assert_contains(verify_volume(machine, 2), "Clean.", "verify of the resumed volume")

    with subtest("a request withdrawn before the write notices it is not honoured"):
        # -clear against a write that is actually running, which is the case it
        # exists for. The writer is frozen between the request and the
        # withdrawal, so this is not a race the implementation happens to win: it
        # gets to look, and there is nothing there.
        mount = load(machine, 3)
        pid, rc = start_write("withdrawn", mount, "-volume", volumes[2])
        machine.log(freeze_once_started(pid, [f"{mount}/data", f"{mount}/containers"]))
        kura(machine, "pause", "-workdir", WORKDIR)
        out = kura(machine, "pause", "-clear", "-workdir", WORKDIR)
        assert_contains(out, "withdrawn", "kura pause -clear")
        machine.fail(f"test -e {REQUEST}")
        thaw(pid)

        status = exit_status(rc)
        assert status == 0, f"a withdrawn request still stopped the write (exit {status})"
        report = machine.succeed("cat /tmp/withdrawn.log")
        assert "=== PAUSED" not in report, f"the write paused after the request was withdrawn:\n{report}"
        assert_contains(report, "=== WROTE", "the write report")
        machine.fail(f"test -e {RESUME}")
        unload(machine)
        assert_contains(kura(machine, "volumes", "-workdir", WORKDIR), volumes[2], "inventory")

    with subtest("a request left over from an earlier run is cleared, not obeyed"):
        # A pause is about the run that was in flight when it was asked for. A
        # write that stopped dead on its first object because of yesterday's
        # request would be a trap, and an operator with a stale flag they had
        # forgotten about would have no way to make any progress at all.
        kura(machine, "pause", "-workdir", WORKDIR)
        out = write_volume(machine, 4, "-quiet")
        assert_contains(out, "clearing a pause request left over from an earlier run", "the stale request")
        assert "=== PAUSED" not in out, f"a stale request stopped a fresh write:\n{out}"
        assert_contains(out, "=== WROTE", "the write report")
        machine.fail(f"test -e {REQUEST}")

    with subtest("what came back is what went in"):
        for slot in range(1, len(volumes) + 1):
            restore_volume(machine, slot, "/tmp/restored")
        assert_trees_identical(machine, STORAGE, restored_path("/tmp/restored"))

    with subtest("a pause on the first volume of a cycle is still a part-written cycle"):
        # The half the main archive cannot prove. Here the catalog holds no
        # records for this generation at all, so without the checkpoint in
        # plan's unfinished-cycle test written would be zero, the refusal would
        # not come, and a new plan would quietly abandon a cartridge holding
        # objects that nothing indexes and no plan can finish.
        kura(
            machine, "plan",
            "-root", STORAGE, "-workdir", SECOND,
            "-capacity", "48MB", "-max-group", "16MB", "-prefix", "NEW",
        )
        second = plan_volumes(machine, plan_path(SECOND))
        assert len(second) >= 2, f"this subtest needs two volumes, plan has {second}"
        blank(machine, 5)
        blank(machine, 6)

        mount = load(machine, 5)
        pid, rc = start_write("second", mount, "-volume", second[0], workdir=SECOND)
        machine.log(freeze_once_started(pid, [f"{mount}/data", f"{mount}/containers"]))
        kura(machine, "pause", "-workdir", SECOND)
        thaw(pid)
        status = exit_status(rc)
        assert status == 75, f"the pause on {second[0]} exited {status}"
        unload(machine)

        out = kura(
            machine, "plan",
            "-root", STORAGE, "-workdir", SECOND,
            "-capacity", "48MB", "-max-group", "16MB", "-prefix", "NEW",
            check=False,
        )
        assert_contains(out, "is only part written", "the refusal for a paused first volume")
        assert_contains(out, second[0], "the refusal")

    with subtest("a pause that names another volume is reported and left alone"):
        # A pause on one cartridge must survive the operator deciding to write a
        # different one, which is an ordinary thing to do: the paused cartridge
        # may be in a drawer, or somebody else's drive. A checkpoint for another
        # volume is not stale, it is about something else, and consuming it would
        # take the one cartridge that can be finished cheaply and turn it into a
        # crash.
        out = write_volume(machine, 6, "-quiet", "-volume", second[1], workdir=SECOND)
        assert_contains(out, f"is volume {second[0]} of generation 1, not {second[1]}", "the note about the pause")
        assert_contains(out, "leaving it alone", "the note about the pause")
        machine.succeed(f"test -s {SECOND_RESUME}")

    with subtest("and it still resumes afterwards, re-read this time"):
        # -verify-resumed reads the adopted objects back and checks them against
        # the digests the pause recorded. Off by default because the mandatory
        # verify pass is the same check done once; what it would catch is
        # resume-trust's subject, and all this asks is that paying for it twice
        # produces the same volume.
        out = write_volume(machine, 5, "-quiet", "-verify-resumed", workdir=SECOND)
        assert_contains(out, "came from a pause and were taken as written, without being read back", "the resume")
        machine.fail(f"test -e {SECOND_RESUME}")
        assert_contains(verify_volume(machine, 5, workdir=SECOND), "Clean.", "verify of the resumed volume")

    with subtest("a second signal gives up waiting and accepts being a crash"):
        # The operator who will not wait for the object in flight — which on the
        # real archive is up to a 10 GiB file, so minutes of apparent deafness.
        # The second signal exits 130 at once and writes no checkpoint, and that
        # is the honest outcome rather than a convenience: nothing was flushed
        # and nothing was recorded, so the next run has to read the medium back
        # to find out what is there, exactly as it would after a kill.
        #
        # Two different signals rather than the same one twice, because a second
        # pending instance of one standard signal is merged while the process is
        # stopped and only one would ever be delivered. And no dwell before the
        # thaw: a signal is not the flag file, so there is no stat window to wait
        # out. The volume is the plan's last, whose entries are the big files —
        # so the writer comes back from the freeze with megabytes still to place,
        # which is what keeps the handler's exit ahead of the ordinary pause path
        # rather than racing it.
        assert len(second) >= 4, f"this subtest needs the plan's fourth volume, plan has {second}"
        blank(machine, 7)
        mount = load(machine, 7)
        pid, rc = start_write("stopnow", mount, "-volume", second[3], workdir=SECOND)
        machine.log(freeze_once_started(pid, [f"{mount}/data", f"{mount}/containers"]))
        quoted = shlex.quote(pid)
        machine.succeed(f'kill -TERM "$(cat {quoted})"', f'kill -HUP "$(cat {quoted})"')
        thaw(pid, dwell=0)

        status = exit_status(rc)
        assert status == 130, f"a second signal must exit 130, not {status}"
        report = machine.succeed("cat /tmp/stopnow.log")
        assert_contains(report, "stopping now", "what the second signal says")
        machine.fail(f"test -e {SECOND_RESUME}")
        unload(machine)

    with subtest("and that volume resumes the expensive way, by reading the medium"):
        # The whole point of the distinction. With nothing vouching for them, the
        # objects already on the cartridge are read back and digested before the
        # write can go on — which is what a pause buys the operator out of.
        out = write_volume(machine, 7, "-quiet", "-volume", second[3], workdir=SECOND)
        assert_contains(out, "were already present from an interrupted run", "the crash resume")
        assert "came from a pause" not in out, (
            f"a run that exited 130 left something behind to be trusted:\n{out}"
        )
        assert_contains(verify_volume(machine, 7, workdir=SECOND), "Clean.", "verify after the crash resume")
  '';
}
