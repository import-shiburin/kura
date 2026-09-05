# Damage on the medium, and what the archive does about it.
#
# The claim being tested is narrow and is the one the design rests on: kura
# corrects nothing, so the only useful behaviour is to notice damage, blame
# exactly the file it destroyed, and get that file written again from the live
# tree on the next cycle. Condemning a whole container for one bad byte would
# be a far worse failure than the byte.
#
# A byte is flipped at a computed offset inside one tar member rather than at a
# random position, so the scenario proves attribution rather than proving that
# something, somewhere, broke.
{ lib, pkgs }:

{
  description = "a flipped byte is caught, attributed to one file, and re-sourced";

  cartridges = [
    192
    192
    192
  ];
  memorySize = 1024;

  script = ''
    machine.start()
    machine.wait_for_unit("multi-user.target")

    with subtest("a written and verified archive"):
        make_source(machine, spec="tiny=200,small=60,medium=3", seed=31)
        kura(
            machine, "plan",
            "-root", STORAGE, "-catalog", CATALOG, "-out", PLAN,
            "-capacity", "64MB", "-max-group", "16MB",
        )
        volumes = plan_volumes(machine)
        for slot in range(1, len(volumes) + 1):
            blank(machine, slot)
            write_volume(machine, slot)
        for slot, vid in enumerate(volumes, start=1):
            assert_contains(verify_volume(machine, slot), "Clean.", f"first verify of {vid}")

    with subtest("one byte inside one container member is flipped"):
        mount = load(machine, 1)
        container = machine.succeed(
            f"find {mount}/containers -name '*.tar' -printf '%s %p\\n' | sort -rn | head -1 | cut -d' ' -f2-"
        ).strip()
        assert container, "no container to damage"
        # Container names carry the grouped item's name, and a grouped range
        # looks like "[s01 .. s03].part001.tar" — spaces and brackets included.
        # Anything in the harness that hands one to a shell has to quote it.
        member = machine.succeed(f"mktree corrupt-member {shlex.quote(container)}").strip()
        damaged = "/" + member
        machine.log(f"damaged {damaged} inside {container}")
        unload(machine)

    with subtest("verify catches it and names the one file it cost"):
        mount = load(machine, 1)
        status, out = kura_status(machine, "verify", "-catalog", CATALOG, "-mount", mount)
        unload(machine)

        assert status != 0, f"verify passed over a corrupted container:\n{out}"
        assert_contains(out, "FAILED to read back as recorded", "verify verdict")
        assert_contains(out, damaged, "the damaged file")
        assert_contains(out, "Marked lost in the catalog", "catalog update")

    with subtest("the blast radius is one file, not the container"):
        # Every other member of that container shares the tar and the volume.
        # If one bad byte condemned all of them the archive would be far more
        # fragile than the medium is.
        failures = [ln for ln in out.splitlines() if ln.strip().startswith("/srv/storage/")]
        assert len(failures) == 1, f"expected one damaged file, got {len(failures)}:\n" + "\n".join(failures)

    with subtest("the inventory reports the loss against the volume that holds it"):
        inventory = kura(machine, "volumes", "-catalog", CATALOG)
        assert_contains(inventory, volumes[0], "inventory")
        machine.log(inventory)

    with subtest("a restore of the damaged volume refuses to look complete"):
        status, out = kura_status(
            machine, "restore", "-catalog", CATALOG,
            "-mount", load(machine, 1), "-into", "/tmp/partial",
        )
        unload(machine)
        assert status != 0, "a restore missing a lost file reported success"

    with subtest("the next plan re-sources the lost file from the live tree"):
        # -start is the operator's job: plan allocates volume IDs from that
        # number without consulting the catalog, because it has no backend to
        # read partly-filled volumes from. That gap is recorded under "Open" in
        # CLAUDE.md; until it closes, a second cycle that omits -start reuses
        # the first cycle's IDs.
        kura(
            machine, "plan",
            "-root", STORAGE, "-catalog", CATALOG, "-out", PLAN,
            "-capacity", "64MB", "-max-group", "16MB",
            "-start", len(volumes) + 1,
        )
        plan_text = machine.succeed(f"cat {PLAN}")
        assert damaged in plan_text, f"generation 2 does not re-source {damaged}"

    with subtest("writing that generation restores the file to the archive"):
        gen2 = plan_volumes(machine)
        slot = len(volumes) + 1
        for vid in gen2:
            blank(machine, slot)
            write_volume(machine, slot)
            assert_contains(verify_volume(machine, slot), "Clean.", f"verify of {vid}")
            slot += 1
  '';
}
