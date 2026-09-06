# The operator never names kura's own files.
#
# Two claims, and they are the same claim from opposite sides.
#
# One: a cycle runs with nothing named. `plan` creates the archive's workdir
# under the state root and leaves the plan in it; every command after that finds
# it again on its own. Naming the catalog on every invocation was never
# something the operator could get right for free — it was kura's bookkeeping
# leaking into the interface.
#
# Two: what that used to allow. A plan and a catalog were independent arguments
# and were never compared, so crossing them stamped one archive's ID and
# another's volume ID onto the same cartridge, recorded the files in a catalog
# belonging to neither, and left `verify` reporting Clean — self-consistent
# after the fact, and undetectable by anything downstream. With the pair
# structural the mistake is hard to construct, and the checks that remain catch
# the one route still open: a plan carried in from elsewhere with -plan.
#
# Every refusal here ends in os.Exit, which no in-process test can survive.
#
# Slots: 1-3 the first archive's opening cycle, 4-5 a second archive over the
# same tree, 6 the first archive's second cycle.
{ lib, pkgs }:

{
  description = "a cycle runs with nothing named, and a plan refuses another archive";

  cartridges = [
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

    # This scenario resolves against the real default root rather than the
    # prelude's named workdir, because "the operator names nothing" is the
    # thing under test.
    ROOT = "/var/lib/kura"
    OTHER = "/var/lib/kura/second"
    CARRIED = "/tmp/carried-plan.json"
    RIVAL = "/tmp/rival-plan.json"

    def archive_dirs():
        """The archive-ID-named workdirs under the root."""
        out = machine.succeed(f"ls -1 {ROOT} 2>/dev/null || true").split()
        return sorted(d for d in out if len(d) == 32 and all(c in "0123456789abcdef" for c in d))

    with subtest("a first plan invents the archive and its workdir"):
        # The workdir under the root is named by the archive, which is what
        # gives a brand-new archive an identity before any cartridge exists:
        # plan has to put its files somewhere and cannot wait for a write.
        make_source(machine, spec="tiny=100,small=40,medium=4", seed=31)
        out = kura(machine, "plan", "-root", STORAGE, "-capacity", "16MB", "-max-group", "8MB")

        dirs = archive_dirs()
        assert len(dirs) == 1, f"expected one archive under {ROOT}, got {dirs}"
        main = f"{ROOT}/{dirs[0]}"
        assert_contains(out, f"plan written to {main}/plan.json", "the plan lands in the workdir")
        machine.succeed(f"test -d {main}/stage")

        p = read_json(machine, f"{main}/plan.json")
        assert p.get("workdir") == main, f"plan names workdir {p.get('workdir')!r}, wanted {main}"
        assert p.get("archive") == dirs[0], f"plan names archive {p.get('archive')!r}, wanted {dirs[0]}"
        assert p.get("id"), "the plan has no ID, so nothing identifies the cycle it starts"
        first_plan_id = p["id"]

        volumes = [v["id"] for v in p["volumes"]]
        assert 2 <= len(volumes) <= 3, f"slots 1-3 are reserved for this cycle, plan wants {volumes}"

    with subtest("the whole cycle runs with only -mount"):
        for slot in range(1, len(volumes) + 1):
            blank(machine, slot)
            mount = load(machine, slot)
            out = kura(machine, "write", "-mount", mount, "-quiet")
            unload(machine)
            assert_contains(out, "=== WROTE", f"volume in slot {slot}")
        machine.succeed(f"test -f {main}/catalog.jsonl.gz")

    with subtest("the reads find it too, with nothing named"):
        assert_contains(kura(machine, "volumes"), volumes[0], "inventory")
        assert_contains(kura(machine, "history"), "GEN", "timeline")
        assert_contains(kura(machine, "at-gen", "1"), "files", "state at generation 1")

    with subtest("the cycle is recorded under the plan that wrote it"):
        entries = [g for g in catalog_header(machine, f"{main}/catalog.jsonl.gz")["timeline"] if g["gen"] == 1]
        assert len(entries) == 1, f"expected one generation-1 entry, got {entries}"
        assert entries[0]["plan"] == first_plan_id, (
            f"generation 1 is recorded under plan {entries[0]['plan']!r}, not {first_plan_id!r}"
        )

    with subtest("re-planning a part-written cycle is refused"):
        # The plan file is kura's now, so a second plan would overwrite the only
        # copy of a cycle with cartridges already on the shelf. No bytes are
        # lost — unwritten files are still new — but the volumes written are
        # stranded in a plan nothing can finish.
        # Enough new data for generation 2 to span cartridges: a cycle that
        # fits on one volume can never be part written, so there would be
        # nothing here to protect.
        make_source(machine, spec="tiny=40,small=20,medium=4", seed=77, root=f"{STORAGE}/more")
        kura(machine, "plan", "-root", STORAGE, "-capacity", "16MB", "-max-group", "8MB")
        next_volumes = [v["id"] for v in read_json(machine, f"{main}/plan.json")["volumes"]]
        assert len(next_volumes) >= 2, f"this subtest needs a multi-volume cycle, got {next_volumes}"

        blank(machine, 6)
        mount = load(machine, 6)
        kura(machine, "write", "-mount", mount, "-quiet")
        unload(machine)

        out = kura(machine, "plan", "-root", STORAGE, "-capacity", "16MB", "-max-group", "8MB", check=False)
        assert_contains(out, "only part written", "re-plan over a live cycle")
        assert_contains(out, "-force", "the way out is offered")

    with subtest("-force gets through, because sometimes that is what is meant"):
        out = kura(machine, "plan", "-root", STORAGE, "-capacity", "16MB", "-max-group", "8MB", "-force")
        assert_contains(out, "plan written", "forced re-plan")

    with subtest("a plan nothing has been written from is replaced freely"):
        # The other half of the rule. Planning twice before committing any
        # media is how an operator reconsiders, and refusing it would make the
        # guard an obstacle rather than a protection.
        kura(machine, "plan", "-root", STORAGE, "-workdir", OTHER, "-capacity", "16MB", "-max-group", "8MB", "-prefix", "OTH")
        machine.succeed(f"cp {OTHER}/plan.json {RIVAL}")
        kura(machine, "plan", "-root", STORAGE, "-workdir", OTHER, "-capacity", "16MB", "-max-group", "8MB", "-prefix", "OTH")

        rival = read_json(machine, RIVAL)
        started = read_json(machine, f"{OTHER}/plan.json")
        assert rival["generation"] == started["generation"], (
            f"the rival plan is generation {rival['generation']}, the started one {started['generation']}"
        )
        assert rival["id"] != started["id"], "two plan runs produced the same ID"
        assert not rival.get("archive"), (
            "the rival plan carries an archive ID, so the cycle check below would not be what proves this"
        )

    with subtest("two archives under the root refuse to be guessed between"):
        # A second archive over the same tree is a real arrangement: DESIGN
        # §8.1 prefers it to erasure coding. Picking one would eventually pick
        # wrong on a command that writes 2.5 TB.
        blank(machine, 4)
        mount = load(machine, 4)
        kura(machine, "write", "-workdir", OTHER, "-mount", mount, "-quiet")
        unload(machine)

        second = catalog_header(machine, f"{OTHER}/catalog.jsonl.gz")["archive"]
        machine.succeed(f"mv {OTHER} {ROOT}/{second}")
        assert len(archive_dirs()) == 2, f"expected two archives, got {archive_dirs()}"

        out = kura(machine, "volumes", check=False)
        assert_contains(out, "holds 2 archives", "ambiguous root")
        for d in archive_dirs():
            assert_contains(out, d, "both candidates named")
        assert_contains(out, "-archive", "the way out is offered")

    with subtest("-archive picks between them, and asserts when a path is also given"):
        assert_contains(kura(machine, "volumes", "-archive", second), second, "selected by archive")

        out = kura(machine, "volumes", "-workdir", main, "-archive", second, check=False)
        assert_contains(out, "holds archive", "mismatched assertion")
        assert_contains(out, second, "mismatched assertion names what was asked for")

    with subtest("a moved workdir still works, and says its plan came from elsewhere"):
        # The workdir was moved under the root a moment ago, so its own plan
        # records a path that no longer exists. That is worth saying out loud
        # rather than passing over: the workdir is what the command mutates and
        # the pairing check below is not total.
        moved = read_json(machine, f"{ROOT}/{second}/plan.json")
        assert moved["workdir"] == OTHER, f"the plan records {moved['workdir']!r}, expected the pre-move path"
        assert len(moved["volumes"]) >= 2, (
            f"the second archive needs a volume left to write, got {[v['id'] for v in moved['volumes']]}"
        )

        blank(machine, 5)
        mount = load(machine, 5)
        out = kura(machine, "write", "-archive", second, "-mount", mount, "-quiet")
        unload(machine)
        assert_contains(out, "this plan was made in", "announced substitution")
        assert_contains(out, "=== WROTE", "a moved workdir still writes")

    with subtest("a plan carried in from another archive is refused"):
        # -plan is the one route left to a crossed pair, and the archive
        # comparison closes it. It needs an established archive on both sides,
        # which is why this waits until the second one has been written to —
        # and why the plan offered is the main archive's, whose workdir is named
        # by its archive and so knew its own ID from the moment it was planned.
        machine.succeed(f"cp {main}/plan.json {CARRIED}")
        carried = read_json(machine, CARRIED)
        assert carried.get("archive") and carried["archive"] != second, (
            f"the carried plan names archive {carried.get('archive')!r}; this proves nothing without a different one"
        )

        out = kura(machine, "write", "-archive", second, "-plan", CARRIED, "-mount", "/mnt/lto", check=False)
        assert_contains(out, "was made against archive", "crossed archives")
        assert_contains(out, "not the same archive", "crossed archives")
  '';
}
