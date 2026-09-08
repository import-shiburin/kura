# What a resumed write is allowed to believe.
#
# interrupted-write proves a killed run converges: the objects it placed are
# recognised rather than rewritten, and the volume ends up verifying. This
# scenario is about what "recognised" may mean. An object already on the
# medium is read back and its digest goes into the catalog — and nothing
# compares it with the source file, or with what the plan described.
#
# Two things can have happened between the interruption and the resume, and
# both are ordinary. The source file can have been edited at the same size,
# which a resume that trusts the medium records as protected with the old
# bytes' digest and the new file's stat data — never to be written again. And
# the object itself can be a casualty of the crash: Put deliberately does not
# fsync, so a power cut can leave an object renamed into place at full size and
# zero-filled, which a resume that trusts the medium digests and records —
# after which verify checks the object against a digest of its own damage.
#
# The scenario does not prescribe what the resume does about either; it asks
# that the archive converge on the source. Report the file and let the next
# cycle write it, or write it again now — both are right. Recording it is not.
#
# Slots 1-3: the plan. 4-5: the cycle that has to follow.
{ lib, pkgs }:

{
  description = "a resumed write never records what it did not check against the source";

  cartridges = [
    256
    256
    256
    256
    256
  ];
  memorySize = 2048;
  diskSize = 6144;

  script = ''
    machine.start()
    machine.wait_for_unit("multi-user.target")

    def rewrite_in_place(machine, path):
        prog = (
            "import os,sys\n"
            "p=sys.argv[1]; st=os.stat(p)\n"
            "b=open(p,'rb').read()\n"
            "t=bytes(x ^ 0x5A for x in range(256))\n"
            "open(p,'wb').write(b.translate(t))\n"
            "os.utime(p, ns=(st.st_atime_ns, st.st_mtime_ns + 2*10**9))\n"
        )
        machine.succeed(f"python3 -c {shlex.quote(prog)} {shlex.quote(path)}")

    def zero_fill(machine, path):
        """The shape a power cut leaves: right size, no content."""
        prog = "import os,sys\np=sys.argv[1]\nn=os.stat(p).st_size\nopen(p,'r+b').write(bytes(n))\n"
        machine.succeed(f"python3 -c {shlex.quote(prog)} {shlex.quote(path)}")

    def write_until_done(first_slot):
        """Blank cartridges, one per remaining volume, until the plan is spent.

        Returns the last slot actually written; the cartridge that met an
        exhausted plan stays blank.
        """
        slot = first_slot
        while True:
            blank(machine, slot)
            mount = load(machine, slot)
            status, out = kura_status(machine, "write", "-workdir", WORKDIR, "-mount", mount, "-quiet")
            unload(machine)
            if "already been written" in out:
                return slot - 1
            assert status == 0, f"writing to slot {slot} failed:\n{out}"
            slot += 1

    with subtest("a tree with enough objects for a kill to land early"):
        make_source(machine, spec="tiny=200,small=120,medium=20", seed=71)
        kura(
            machine, "plan",
            "-root", STORAGE, "-workdir", WORKDIR,
            "-capacity", "56MB", "-max-group", "16MB",
        )
        volumes = plan_volumes(machine)
        assert len(volumes) >= 3, f"this scenario needs three volumes, plan has {volumes}"
        for slot in range(1, len(volumes) + 1):
            blank(machine, slot)
        write_volume(machine, 1, "-quiet")

    with subtest("a write is killed after a few objects"):
        mount = load(machine, 2)
        start_background(
            machine,
            f"kura write -workdir {WORKDIR} -mount {mount} -quiet -volume {volumes[1]}",
            "/tmp/write.pid",
            "/tmp/write.log",
        )
        machine.log(kill_once_started(machine, "/tmp/write.pid", [f"{mount}/data", f"{mount}/containers"]))
        placed = machine.succeed(f"find {mount}/data -type f | sort").split()
        assert len(placed) >= 2, f"the kill landed before two data objects were placed: {placed}"

    with subtest("between the kill and the resume, the medium and the source both change"):
        # One placed object did not survive the crash.
        damaged = placed[0]
        zero_fill(machine, damaged)
        # Another placed object's source file was edited in the meantime. The
        # object name mirrors the source path: data/<path without its leading
        # slash>.
        edited_object = placed[1]
        edited_source = "/" + os.path.relpath(edited_object, f"{mount}/data")
        machine.succeed(f"test -f {shlex.quote(edited_source)}")
        rewrite_in_place(machine, edited_source)
        unload(machine)

    with subtest("the resume runs, and the rest of the plan is written"):
        mount = load(machine, 2)
        status, out = kura_status(machine, "write", "-workdir", WORKDIR, "-mount", mount, "-quiet")
        unload(machine)
        machine.log(f"resume: status {status}\n{out}")
        last = write_until_done(3)

    with subtest("the next cycle re-sources whatever the resume could not vouch for"):
        kura(
            machine, "plan",
            "-root", STORAGE, "-workdir", WORKDIR,
            "-capacity", "56MB", "-max-group", "16MB",
            "-start", last + 1,
        )
        last = write_until_done(last + 1)

    with subtest("what comes back is the source as it is now"):
        for s in range(1, last + 1):
            restore_volume(machine, s, "/tmp/restored")
        assert_trees_identical(machine, STORAGE, restored_path("/tmp/restored"))
  '';
}
