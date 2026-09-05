# The cycle from README/CLAUDE.md, performed the way an operator performs it.
#
# plan once, then write once per cartridge with a real eject and insert in
# between, then verify each, then restore and compare byte for byte.
#
# The in-process tests already round-trip an archive. What they cannot do is
# run each volume as its own process against a medium with a real capacity:
# every `kura write` here starts, writes one volume, and exits before the next
# cartridge is mounted, which is the contract the whole tool is built around.
{ lib, pkgs }:

{
  description = "plan, write across cartridges, verify, restore, compare";

  # Five cartridges for a plan that should want three. The spare ones are the
  # point of owning spares, and a scenario that provisions exactly as many as
  # it needs cannot tell "used them all" from "ran out".
  cartridges = [
    192
    192
    192
    192
    192
  ];
  memorySize = 2048;
  diskSize = 6144;

  script = ''
    machine.start()
    machine.wait_for_unit("multi-user.target")

    with subtest("a source tree shaped like the real archive"):
        files, total = make_source(machine, spec="tiny=250,small=90,medium=5,large=2", seed=11)
        assert files == 347, f"generator produced {files} files, expected 347"
        machine.log(f"source: {files} files, {total} bytes")

    with subtest("plan lays the tree out across several volumes"):
        out = kura(
            machine, "plan",
            "-root", STORAGE,
            "-catalog", CATALOG,
            "-out", PLAN,
            "-capacity", "96MB",
            "-max-group", "64MB",
            "-tar-below", "1MB",
        )
        assert_contains(out, "plan written to", "plan output")
        volumes = plan_volumes(machine)
        assert 2 <= len(volumes) <= 5, f"plan wants {len(volumes)} volumes: {volumes}"
        machine.log(f"plan: {volumes}")

    with subtest("each volume is written by its own process, with a swap in between"):
        for slot, vid in enumerate(volumes, start=1):
            blank(machine, slot)
            write_volume(machine, slot, vid)
        # Nothing is mounted between runs, so nothing could have been held open
        # across the swap.
        machine.fail("mountpoint -q /mnt/lto/" + volumes[0])

    with subtest("the on-media layout is what DESIGN describes"):
        mount = load(machine, 1, volumes[0])
        listing = machine.succeed(f"cd {mount} && find . -maxdepth 2 | sort")
        assert_contains(listing, "./kura.archive.json", "archive header")
        assert_contains(listing, "./kura.volume.json", "volume header")
        assert_contains(listing, "./generations/g0001", "per-generation metadata")
        assert_contains(listing, "./data", "data objects")

        # The root carries write-once identity and nothing else. Anything with a
        # mutable field would have to be rewritten every cycle, and on this
        # medium a rewrite abandons the old blocks rather than replacing them.
        header = read_json(machine, f"{mount}/kura.volume.json")
        assert header["magic"] == "kura-volume", header
        assert header["volume"] == volumes[0], header
        assert set(header) == {"magic", "format", "archive", "volume"}, (
            f"the volume header grew a field that will have to be rewritten: {sorted(header)}"
        )

        archive_header = read_json(machine, f"{mount}/kura.archive.json")
        assert archive_header["magic"] == "kura-archive", archive_header
        assert archive_header["archive"] == header["archive"], (
            f"the two headers disagree: {archive_header['archive']} vs {header['archive']}"
        )

        # Written twice, so a volume can lose one copy of its index and still
        # describe itself (DESIGN §8.1a).
        copies = int(machine.succeed(
            f"ls -1 {mount}/generations/g0001/catalog.*.jsonl.gz | wc -l"
        ).strip())
        assert copies >= 2, f"volume carries {copies} catalog copies, expected at least 2"
        unload(machine)

    with subtest("small files went into containers, large files did not"):
        mount = load(machine, 1, volumes[0])
        containers = machine.succeed(f"find {mount}/containers -name '*.tar' | wc -l").strip()
        assert int(containers) > 0, "no tar containers were written"
        unload(machine)

    with subtest("the inventory knows the volumes and that none has been verified"):
        out = kura(machine, "volumes", "-catalog", CATALOG)
        for vid in volumes:
            assert_contains(out, vid, "volume inventory")
        assert_contains(out, "never", "unverified volumes")

    with subtest("every volume reads back clean"):
        for slot, vid in enumerate(volumes, start=1):
            out = verify_volume(machine, slot, vid)
            assert_contains(out, "Clean.", f"verify of {vid}")

    with subtest("verification is recorded, so the inventory stops nagging"):
        out = kura(machine, "volumes", "-catalog", CATALOG)
        assert "never" not in out, f"a volume is still unverified after a full pass:\n{out}"

    with subtest("restoring every volume reproduces the tree byte for byte"):
        for slot, vid in enumerate(volumes, start=1):
            restore_volume(machine, slot, vid, "/tmp/restored")
        assert_trees_identical(machine, STORAGE, restored_path("/tmp/restored"))

    with subtest("history shows the one generation this cycle made"):
        out = kura(machine, "history", "-catalog", CATALOG)
        assert_contains(out, "1", "generation timeline")

    with subtest("a later generation appended to a written cartridge gets its own index"):
        # Regression. The catalog copies were named by copy number alone, so a
        # second generation found kura.catalog.000/001 already taken and
        # skipped them, leaving the cartridge carrying an index that described
        # only the first cycle. Silent, because nothing reads the on-volume
        # catalog back until the local one is gone — the one moment it has to
        # be right.
        mount = load(machine, 1, volumes[0])
        before = machine.succeed(f"sha256sum {mount}/generations/g0001/catalog.000.jsonl.gz").split()[0]
        unload(machine)

        mktree(machine, "mutate", STORAGE, "--seed", 77, "--add", 3)
        kura(
            machine, "plan",
            "-root", STORAGE, "-catalog", CATALOG, "-out", PLAN,
            "-capacity", "96MB", "-max-group", "64MB", "-start", 1,
        )
        later = plan_volumes(machine)
        write_volume(machine, 1, later[0], "-quiet")

        mount = load(machine, 1, later[0])
        generations = machine.succeed(f"ls {mount}/generations").split()
        after = machine.succeed(f"sha256sum {mount}/generations/g0001/catalog.000.jsonl.gz").split()[0]
        unload(machine)

        assert "g0001" in generations and "g0002" in generations, (
            f"the cartridge should carry one index per generation, has: {generations}"
        )
        # Write-once holds: the earlier index is still there to fall back on,
        # which is the whole reason a rewrite would have been wrong.
        assert before == after, "appending generation 2 rewrote generation 1's catalog"

    with subtest("the appended volume still verifies and restores"):
        assert_contains(verify_volume(machine, 1, later[0]), "Clean.", "verify after appending")
        restore_volume(machine, 1, later[0], "/tmp/appended")
  '';
}
