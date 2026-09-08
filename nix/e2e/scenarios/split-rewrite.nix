# A file that spans cartridges, rewritten between them.
#
# A split file is the one kind of file the writer only ever sees a piece of at
# a time, days apart on real hardware. Each fragment is digested on its own,
# and that is the right granularity for verifying one cartridge at a time — but
# the fragments have to be pieces of one file. Whole objects are checked against
# the plan before they are read; fragments are not, and their record carries
# none of the ctime, inode or mode a whole file's record does.
#
# The rewrite here keeps the size and puts the mtime back, which on a real
# filesystem leaves ctime as the only stat-level trace: the case DESIGN §6 is
# built around, and the one a record without a ctime cannot see.
#
# Slots 1-3: the first generation. 4-6: the cycle that has to follow.
{ lib, pkgs }:

{
  description = "a split file rewritten between cartridges is never stitched from two versions";

  cartridges = [
    48
    48
    48
    48
    48
    48
  ];
  memorySize = 1024;
  diskSize = 4096;

  script = ''
    machine.start()
    machine.wait_for_unit("multi-user.target")

    ORIGINAL = "/tmp/original.bin"

    def rewrite_keeping_mtime(machine, path):
        prog = (
            "import os,sys\n"
            "p=sys.argv[1]; st=os.stat(p)\n"
            "b=open(p,'rb').read()\n"
            "t=bytes(x ^ 0x5A for x in range(256))\n"
            "open(p,'wb').write(b.translate(t))\n"
            "os.utime(p, ns=(st.st_atime_ns, st.st_mtime_ns))\n"
        )
        machine.succeed(f"python3 -c {shlex.quote(prog)} {shlex.quote(path)}")

    def digest(machine, path):
        return machine.succeed(f"sha256sum {shlex.quote(path)}").split()[0]

    def record_of(machine, path):
        """The catalog's newest record for a path, read in the guest."""
        prog = (
            "import gzip,json,sys\n"
            "best=None\n"
            "for line in gzip.open(sys.argv[1],'rt'):\n"
            "    r=json.loads(line)\n"
            "    if r.get('path')==sys.argv[2] and (best is None or r.get('gen',0)>=best.get('gen',0)): best=r\n"
            "print(json.dumps(best))\n"
        )
        return json.loads(machine.succeed(
            f"python3 -c {shlex.quote(prog)} {shlex.quote(catalog_path())} {shlex.quote(path)}"
        ))

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

    with subtest("one file larger than a cartridge"):
        make_source(machine, spec="large=1", seed=81)
        path = machine.succeed(f"find {STORAGE} -type f").strip()
        machine.succeed(f"cp {shlex.quote(path)} {ORIGINAL}")
        kura(
            machine, "plan",
            "-root", STORAGE, "-workdir", WORKDIR,
            "-capacity", "24MB", "-allow-split", "-split-min", "1MB",
        )
        volumes = plan_volumes(machine)
        assert 2 <= len(volumes) <= 3, f"expected the file to span 2 or 3 cartridges, plan has {volumes}"

    with subtest("the first cartridge is written, then the file is rewritten in place"):
        blank(machine, 1)
        write_volume(machine, 1, "-quiet")
        rewrite_keeping_mtime(machine, path)
        assert digest(machine, path) != digest(machine, ORIGINAL)

    with subtest("the remaining cartridges are offered the new bytes"):
        statuses = []
        for slot in range(2, len(volumes) + 1):
            blank(machine, slot)
            mount = load(machine, slot)
            status, out = kura_status(machine, "write", "-workdir", WORKDIR, "-mount", mount, "-quiet")
            unload(machine)
            machine.log(f"slot {slot}: status {status}\n{out}")
            statuses.append(status)

    with subtest("no restore stitches the two versions together"):
        outputs = []
        for slot in range(1, len(volumes) + 1):
            mount = load(machine, slot)
            machine.succeed("mkdir -p /tmp/restore")
            status, out = kura_status(
                machine, "restore", "-workdir", WORKDIR, "-mount", mount, "-into", "/tmp/restore",
            )
            unload(machine)
            outputs.append((status, out))
        restored = os.path.join(restored_path("/tmp/restore"), os.path.relpath(path, STORAGE))
        present, _ = machine.execute(f"test -f {shlex.quote(restored)}")
        if present == 0:
            got = digest(machine, restored)
            assert got in (digest(machine, ORIGINAL), digest(machine, path)), (
                "the restored file is neither the version planned nor the version on disk: "
                "fragments of two versions were stitched together and every per-part digest agreed"
            )
        else:
            said_so = any(status != 0 or "partly written" in out or "FAILED" in out for status, out in outputs)
            assert said_so, "the file was not restored and no restore said why"

    with subtest("the split file's record carries what change detection needs"):
        rec = record_of(machine, path)
        assert rec is not None, f"no record for {path}"
        assert rec.get("ctime"), f"the split record has no ctime, so a rewrite behind a restored mtime is invisible to it: {rec}"
        assert rec.get("inode"), f"the split record has no inode: {rec}"

    with subtest("the next cycle sees the rewrite and writes it"):
        out = kura(
            machine, "plan",
            "-root", STORAGE, "-workdir", WORKDIR,
            "-capacity", "24MB", "-allow-split", "-split-min", "1MB",
            "-start", len(volumes) + 1,
        )
        counts = delta_counts(out)
        assert counts.get("unchanged", 0) == 0, (
            f"the rewritten file was settled as unchanged; its mtime was restored and nothing else was checked: {counts}"
        )
        last = write_until_done(len(volumes) + 1)

    with subtest("what comes back is the file as it is now"):
        for s in range(1, last + 1):
            restore_volume(machine, s, "/tmp/restore-now")
        assert_trees_identical(machine, STORAGE, restored_path("/tmp/restore-now"))
  '';
}
