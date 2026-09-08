# Volume IDs that collide, and what identity checks do about it.
#
# Every `kura plan` numbers its volumes from -start, whose default is 1, and
# every archive uses the same default prefix. So a second generation names
# LTO001 again, and a second archive on the same host does too. The other
# scenarios pass -start and -prefix by hand to stay out of this; this one runs
# the commands the way the usage text shows them and asks what has to hold
# regardless of how IDs are handed out.
#
# Three things. A plan must not name a volume the catalog already lists as
# written. A written cartridge must never be appended to in a way that records
# an old object under a new generation. And the identity checks that write
# already performs — is this cartridge ours, is it the one the plan means —
# have to be performed by every command that touches a cartridge, and before
# anything is adopted from it.
#
# Slots 1-3: archive A's first generation. 4: A's second. 5: archive B.
# 6-7: spares.
{ lib, pkgs }:

{
  description = "colliding volume IDs cannot lose data or hijack an archive";

  cartridges = [
    128
    128
    128
    128
    128
    128
    128
  ];
  memorySize = 1024;

  script = ''
    machine.start()
    machine.wait_for_unit("multi-user.target")

    SECOND = "/var/lib/kura/second"
    THIRD = "/var/lib/kura/third"

    def rewrite_in_place(machine, path):
        """Same length, different bytes, mtime moved on: an ordinary edit."""
        prog = (
            "import os,sys\n"
            "p=sys.argv[1]; st=os.stat(p)\n"
            "b=open(p,'rb').read()\n"
            "t=bytes(x ^ 0x5A for x in range(256))\n"
            "open(p,'wb').write(b.translate(t))\n"
            "os.utime(p, ns=(st.st_atime_ns, st.st_mtime_ns + 2*10**9))\n"
        )
        machine.succeed(f"python3 -c {shlex.quote(prog)} {shlex.quote(path)}")

    def written_ids(workdir):
        return [v["id"] for v in catalog_header(machine, catalog_path(workdir)).get("volumes", [])]

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

    with subtest("archive A writes its first generation with the defaults"):
        make_source(machine, spec="tiny=60,small=30,medium=3", seed=61)
        kura(
            machine, "plan",
            "-root", STORAGE, "-workdir", WORKDIR,
            "-capacity", "16MB", "-max-group", "8MB",
        )
        gen1 = plan_volumes(machine)
        assert 2 <= len(gen1) <= 3, f"slots 1-3 are reserved for this cycle, plan wants {gen1}"
        for slot, vid in enumerate(gen1, start=1):
            blank(machine, slot)
            write_volume(machine, slot)
        assert sorted(written_ids(WORKDIR)) == sorted(gen1), written_ids(WORKDIR)

    with subtest("a second cycle allocates volume IDs the archive has not used"):
        # The edited file is one the first cartridge holds, so that a plan
        # which names that cartridge's ID again meets an object already under
        # the file's name. Object names mirror source paths.
        mount = load(machine, 1)
        held = machine.succeed(f"find {mount}/data -name 'medium-*' | sort | sed -n 1p").strip()
        assert held, "the first cartridge holds no medium-sized file"
        edited = "/" + os.path.relpath(held, f"{mount}/data")
        unload(machine)
        rewrite_in_place(machine, edited)
        kura(
            machine, "plan",
            "-root", STORAGE, "-workdir", WORKDIR,
            "-capacity", "16MB", "-max-group", "8MB",
        )
        gen2 = plan_volumes(machine)
        assert gen2, "generation 2 has nothing to write"
        clash = sorted(set(gen2) & set(written_ids(WORKDIR)))
        assert not clash, (
            f"generation 2 plans to write {clash}, which the catalog already lists as written: "
            f"two cartridges would answer to one ID"
        )

    with subtest("the generation-1 cartridge in the drive cannot swallow the edit"):
        # Whatever the plan named, loading the cartridge that already holds
        # the old version must not end with a generation-2 record pointing at
        # generation-1 bytes. A refusal is fine; so is a real write. The
        # restore at the end is what decides.
        mount = load(machine, 1)
        status, out = kura_status(machine, "write", "-workdir", WORKDIR, "-mount", mount, "-quiet")
        unload(machine)
        machine.log(f"write onto the gen-1 cartridge: status {status}\n{out}")
        last = write_until_done(len(gen1) + 1)

        # Every cartridge the catalog vouches for has to restore, and what
        # they produce has to be the tree as it is now. A restore that fails
        # here means a record points at bytes that are not on the cartridge
        # carrying that ID — two cartridges answering to one name.
        failures = []
        for s in range(1, last + 1):
            mount = load(machine, s)
            machine.succeed("mkdir -p /tmp/restore-gen2")
            status, out = kura_status(
                machine, "restore", "-workdir", WORKDIR, "-mount", mount, "-into", "/tmp/restore-gen2",
            )
            unload(machine)
            if status != 0:
                failures.append(f"slot {s}:\n{out}")
        assert not failures, (
            "restoring generation 2 failed on a cartridge the catalog vouches for:\n" + "\n".join(failures)
        )
        assert_trees_identical(machine, STORAGE, restored_path("/tmp/restore-gen2"))

    with subtest("archive B writes its first cartridge, also LTO001"):
        blank(machine, 5)
        kura(
            machine, "plan",
            "-root", STORAGE, "-workdir", SECOND,
            "-capacity", "16MB", "-max-group", "8MB",
        )
        mount = load(machine, 5)
        kura(machine, "write", "-workdir", SECOND, "-mount", mount, "-quiet")
        unload(machine)
        b_archive = catalog_header(machine, catalog_path(SECOND))["archive"]
        assert written_ids(SECOND)[0] == gen1[0], (
            f"this scenario needs both archives to name their first cartridge alike, "
            f"got {written_ids(SECOND)} and {gen1}"
        )

    with subtest("a new archive's first write refuses another archive's cartridge"):
        # Archive C has never written anything, so its catalog has no ID and
        # the only evidence of who it is lives in the plan its workdir was
        # made for. The cartridge in the drive is B's.
        kura(
            machine, "plan",
            "-root", STORAGE, "-workdir", THIRD,
            "-capacity", "16MB", "-max-group", "8MB",
        )
        mount = load(machine, 5)
        before = count_files(machine, f"{mount}/data", f"{mount}/containers")
        status, out = kura_status(machine, "write", "-workdir", THIRD, "-mount", mount, "-quiet")
        after = count_files(machine, f"{mount}/data", f"{mount}/containers")
        unload(machine)
        assert status != 0, f"archive C wrote onto archive B's cartridge:\n{out}"
        assert after == before, f"archive C placed {after - before} objects on archive B's cartridge"
        exists, _ = machine.execute(f"test -e {catalog_path(THIRD)}")
        if exists == 0:
            c_archive = catalog_header(machine, catalog_path(THIRD)).get("archive")
            assert c_archive != b_archive, "archive C adopted archive B's identity"

    with subtest("verify refuses another archive's cartridge"):
        mount = load(machine, 5)
        status, out = kura_status(machine, "verify", "-workdir", WORKDIR, "-mount", mount)
        unload(machine)
        assert status != 0, f"archive A verified archive B's cartridge as its own:\n{out}"
        # Nothing of A's was marked lost: at-gen exits non-zero on any hole.
        kura(machine, "at-gen", "-workdir", WORKDIR, "1")
        kura(machine, "at-gen", "-workdir", WORKDIR, "2")
  '';
}
