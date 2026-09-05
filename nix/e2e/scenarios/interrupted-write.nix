# A write that does not get to finish.
#
# Two interruptions, because they break different promises.
#
# SIGKILL kills the process while the medium survives: every object the writer
# had renamed into place is there, and a re-run has to recognise them rather
# than fail on the first name it already used. That is what `existing()` is
# for.
#
# A power cut takes the page cache with it. Put deliberately does not fsync —
# an fsync per object would cost over an hour across a real archive's 1.66 M
# objects — so durability comes from the Sync at the end of a volume, and the
# guarantee before that point is atomicity, not persistence. What must hold is
# that a re-run converges on a volume that verifies and restores byte for byte,
# whatever the crash left behind.
#
# Neither interruption leaves coverage counters for the process it killed: Go
# flushes them on exit, and neither of these is an exit. The counters from the
# runs on either side are collected as usual.
{ lib, pkgs }:

{
  description = "a killed write and a power cut both resume to a verifiable volume";

  cartridges = [
    256
    256
    256
    128 # a fresh archive, for the first-write hazard below
  ];
  memorySize = 2048;
  diskSize = 6144;

  script = ''
    machine.start()
    machine.wait_for_unit("multi-user.target")

    with subtest("a tree with enough objects that a kill lands early in the run"):
        # Objects, not bytes, are what gives the interruption its margin: the
        # watcher fires two objects in, and there need to be many more after
        # that for the word "mid-write" to mean anything.
        make_source(machine, spec="tiny=200,small=120,medium=20", seed=61)
        kura(
            machine, "plan",
            "-root", STORAGE, "-catalog", CATALOG, "-out", PLAN,
            "-capacity", "56MB", "-max-group", "16MB",
        )
        volumes = plan_volumes(machine)
        assert len(volumes) >= 3, f"this scenario needs three volumes, plan has {volumes}"
        machine.log(f"plan: {volumes}")

    with subtest("the first volume establishes the archive"):
        # Deliberately not the volume that gets interrupted. Until a write
        # finishes, the archive ID exists only in that process's memory — see
        # the last subtest, which is about exactly that.
        for slot in range(1, len(volumes) + 1):
            blank(machine, slot)
        write_volume(machine, 1, volumes[0], "-quiet")
        assert_contains(kura(machine, "volumes", "-catalog", CATALOG), volumes[0], "inventory")

    with subtest("a write killed mid-flight leaves objects but no catalog record"):
        mount = load(machine, 2, volumes[1])
        start_background(
            machine,
            f"kura write -plan {PLAN} -catalog {CATALOG} -drive {mount} -quiet -volume {volumes[1]}",
            "/tmp/write1.pid",
            "/tmp/write1.log",
        )
        machine.log(kill_once_started(machine, "/tmp/write1.pid", [f"{mount}/data", f"{mount}/containers"]))

        placed = count_files(machine, f"{mount}/data", f"{mount}/containers")
        assert placed >= 2, f"the kill landed before anything was written ({placed} objects)"
        unload(machine)

        # This is also the proof that the kill landed mid-write rather than
        # after it: archive.Write returns before the catalog is written, so a
        # run that finished would have recorded this volume.
        inventory = kura(machine, "volumes", "-catalog", CATALOG)
        assert volumes[1] not in inventory, (
            f"{volumes[1]} was recorded despite the kill; the write outran it:\n{inventory}"
        )

    with subtest("re-running the same write resumes over what is already there"):
        # Put refuses to overwrite a name, so this only works if the writer
        # recognises its own earlier objects.
        out = write_volume(machine, 2, volumes[1], "-quiet")
        assert "already exists" not in out, f"resume tried to rewrite an object:\n{out}"
        assert_contains(verify_volume(machine, 2, volumes[1]), "Clean.", "verify after resume")

    with subtest("a power cut during the next volume is survivable"):
        mount = load(machine, 3, volumes[2])
        start_background(
            machine,
            f"kura write -plan {PLAN} -catalog {CATALOG} -drive {mount} -quiet -volume {volumes[2]}",
            "/tmp/write2.pid",
            "/tmp/write2.log",
        )
        wait_for(machine, f"test $(find {mount}/data {mount}/containers -type f 2>/dev/null | wc -l || true) -ge 1")
        machine.crash()

        machine.start()
        machine.wait_for_unit("multi-user.target")

    with subtest("after the reboot the volume can be finished and verifies"):
        # The journal replays, the medium holds whatever was durable, and the
        # writer picks up from there. Nothing here asserts how much survived —
        # that is the medium's business — only that the archive converges.
        write_volume(machine, 3, volumes[2], "-quiet")
        assert_contains(verify_volume(machine, 3, volumes[2]), "Clean.", "verify after the power cut")

    with subtest("what came back is what went in"):
        for slot, vid in enumerate(volumes, start=1):
            restore_volume(machine, slot, vid, "/tmp/restored")
        assert_trees_identical(machine, STORAGE, restored_path("/tmp/restored"))

    with subtest("an interrupted FIRST write can be finished, not orphaned"):
        # Regression. The archive ID used to be minted in memory, stamped onto
        # the medium by archive.Reconcile before the first object, and saved to
        # the local catalog only once the whole volume succeeded. Any
        # interruption in between lost it: the next run minted a different ID
        # and refused the cartridge it had itself stamped, permanently, and the
        # window was the entire first volume of a new archive.
        #
        # Two things close it, and this checks both. The ID is persisted before
        # any object is written, and the identity now lives in an object of its
        # own so a volume can hand it back.
        FRESH = "/var/lib/kura/fresh.jsonl.gz"
        FRESH_PLAN = "/var/lib/kura/fresh-plan.json"
        kura(
            machine, "plan",
            "-root", STORAGE, "-catalog", FRESH, "-out", FRESH_PLAN,
            "-capacity", "56MB", "-max-group", "16MB", "-prefix", "NEW",
        )
        fresh = plan_volumes(machine, FRESH_PLAN)

        blank(machine, 4)
        mount = load(machine, 4, fresh[0])
        start_background(
            machine,
            f"kura write -plan {FRESH_PLAN} -catalog {FRESH} -drive {mount} -quiet -volume {fresh[0]}",
            "/tmp/write3.pid",
            "/tmp/write3.log",
        )
        machine.log(kill_once_started(machine, "/tmp/write3.pid", [f"{mount}/data", f"{mount}/containers"]))

        stamped = read_json(machine, f"{mount}/kura.archive.json")
        assert stamped["archive"], "the medium was not stamped before objects were written"

        # The catalog exists and names the same archive, despite the kill.
        header = catalog_header(machine, FRESH)
        assert header.get("archive") == stamped["archive"], (
            f"catalog names {header.get('archive')!r}, the medium names {stamped['archive']!r}"
        )

        kura(machine, "write", "-plan", FRESH_PLAN, "-catalog", FRESH,
             "-drive", mount, "-volume", fresh[0], "-quiet")
        unload(machine)

        mount = load(machine, 4, fresh[0])
        assert_contains(kura(machine, "verify", "-catalog", FRESH, "-drive", mount), "Clean.", "verify of the finished volume")
        unload(machine)

    with subtest("a catalog that lost its archive ID adopts the medium's"):
        # The stronger half: the medium is the authority (DESIGN §11), so even
        # a catalog destroyed outright is repaired from any cartridge rather
        # than minting a second identity for an archive that already has one.
        machine.succeed(f"rm {FRESH}")
        mount = load(machine, 4, fresh[0])
        out = kura(machine, "write", "-plan", FRESH_PLAN, "-catalog", FRESH,
                   "-drive", mount, "-volume", fresh[0], "-quiet")
        unload(machine)
        assert_contains(out, "adopting archive " + stamped["archive"], "adoption from the medium")
  '';
}
