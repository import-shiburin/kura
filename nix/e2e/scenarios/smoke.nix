# The harness testing itself.
#
# Every other scenario assumes cartridges appear where the base module says
# they do, that kura is on PATH, and that coverage counters come back out. When
# one of those breaks, every scenario fails at once and the logs all blame the
# archive rather than the harness. This one fails alone, and first.
#
# It also covers the argument-handling paths that unit tests structurally
# cannot reach: every one of them ends in os.Exit, which no in-process test can
# survive, and all of them are in cmd/kura — the package with the largest
# coverage gap.
{ lib, pkgs }:

{
  description = "the harness is sound: cartridges, binary, coverage collection";

  cartridges = [
    256
    256
  ];
  memorySize = 1024;

  script = ''
    machine.start()
    machine.wait_for_unit("multi-user.target")

    with subtest("cartridges are addressable by slot, not by kernel device order"):
        machine.succeed("test -e /dev/disk/by-id/virtio-kura-slot-1")
        machine.succeed("test -e /dev/disk/by-id/virtio-kura-slot-2")
        machine.log(machine.succeed("lsblk -o NAME,SIZE,SERIAL"))

    with subtest("a cartridge can be formatted, loaded, and ejected"):
        # One drive, one path, whatever is in it. The mount point says nothing
        # about which volume this is; that is read from the medium.
        blank(machine, 1)
        mount = load(machine, 1)
        assert mount == "/mnt/lto", f"mounted at {mount}"
        machine.succeed("mountpoint -q /mnt/lto")
        unload(machine)
        machine.fail("mountpoint -q /mnt/lto")

    with subtest("the fixture generator is deterministic across trees"):
        make_source(machine, spec="tiny=8,small=4", seed=7, root="/tmp/a")
        make_source(machine, spec="tiny=8,small=4", seed=7, root="/tmp/b")
        machine.succeed("diff -r /tmp/a /tmp/b")

    with subtest("kura runs and reports its version"):
        assert_contains(kura(machine, "version"), "kura ", "version output")

    # These are the os.Exit paths. They are worth naming individually because
    # each is a distinct decision the CLI makes about bad input, and because
    # coverage for all of them arrives only from running the real binary.
    with subtest("an unknown subcommand is refused with usage"):
        out = kura(machine, "definitely-not-a-command", check=False)
        assert_contains(out, "unknown command", "unknown-command error")
        assert_contains(out, "Usage:", "usage text")

    with subtest("help is not an error"):
        assert_contains(kura(machine, "help"), "Commands:", "help output")

    with subtest("required flags are enforced rather than defaulted"):
        assert_contains(kura(machine, "write", check=False), "required", "write with no flags")
        assert_contains(kura(machine, "verify", check=False), "required", "verify with no flags")
        assert_contains(kura(machine, "restore", check=False), "required", "restore with no flags")

    with subtest("restoring from a catalog that holds nothing says so"):
        # A missing catalog loads as an empty one rather than failing, so this
        # is the first run's most likely mistake: the operator restores before
        # anything has been written. It has to name the reason, not the file.
        out = kura(
            machine, "restore", "-catalog", "/var/lib/kura/nothing-here.jsonl.gz",
            "-mount", "/tmp/no-such-mount", "-into", "/tmp/nowhere", check=False,
        )
        assert_contains(out, "nothing to restore", "empty catalog")

    with subtest("per-command help lists that command's flags"):
        for cmd in ["plan", "write", "verify", "restore", "volumes", "history", "at-gen"]:
            status, out = kura_status(machine, cmd, "-h")
            assert "-" in out, f"`kura {cmd} -h` printed nothing useful:\n{out}"

    with subtest("running with no arguments at all prints usage and fails"):
        status, out = machine.execute("kura 2>&1")
        assert status != 0, "bare `kura` should exit non-zero"
        assert_contains(out, "Usage:", "bare invocation")

    # The optional flags. Each is a branch that only a real invocation reaches,
    # and between them they are most of what plan can be asked to do beyond its
    # defaults.
    with subtest("plan's optional outputs and modes"):
        make_source(machine, spec="tiny=20,small=6", seed=3)
        machine.succeed(f"touch {STORAGE}/notes.skipme")
        machine.succeed("printf '%s\\n' '*.skipme' > /tmp/ex.txt")

        out = kura(
            machine, "plan",
            "-root", STORAGE, "-catalog", CATALOG, "-out", PLAN,
            "-lists", "/tmp/lists",
            "-exclude-from", "/tmp/ex.txt",
            "-hash", "all",
            "-capacity", "16MB", "-top-depth", "1",
        )
        assert_contains(out, "per-volume file lists written to", "-lists")
        assert_contains(out, "hashing", "-hash all")

        volumes = plan_volumes(machine)
        for vid in volumes:
            machine.succeed(f"test -s /tmp/lists/{vid}.files")
        # The exclusion took effect rather than being silently accepted.
        assert count_files(machine, "/tmp/lists") == len(volumes)
        machine.succeed(f"! grep -q skipme /tmp/lists/{volumes[0]}.files")

    with subtest("bad values for plan's enumerated flags are refused"):
        for flag, value in [("-hash", "sideways"), ("-match", "sideways")]:
            out = kura(machine, "plan", "-root", STORAGE, "-catalog", CATALOG,
                       "-out", "-", flag, value, check=False)
            assert_contains(out, "must be", f"{flag} {value}")

    with subtest("plan with no -root is refused"):
        out = kura(machine, "plan", "-catalog", CATALOG, "-out", "-", check=False)
        assert_contains(out, "at least one -root is required", "missing -root")

    with subtest("at-gen wants exactly one generation"):
        for args in ([], ["1", "2"]):
            status, out = kura_status(machine, "at-gen", "-catalog", CATALOG, *args)
            assert status != 0, f"at-gen accepted {args!r}:\n{out}"

    with subtest("volumes flags an overdue verification"):
        blank(machine, 1)
        write_volume(machine, 1, "-quiet")
        assert_contains(verify_volume(machine, 1), "Clean.", "verify")

        # A zero window makes a volume verified a moment ago already overdue,
        # which is the only way to reach that branch without waiting months.
        out = kura(machine, "volumes", "-catalog", CATALOG, "-overdue", "0s")
        assert_contains(out, "overdue", "overdue window")
        assert_contains(out, "Most overdue first", "overdue advice")

        status, out = kura_status(machine, "volumes", "-catalog", CATALOG, "-overdue", "weekly")
        assert status != 0, f"an unparseable window was accepted:\n{out}"

    with subtest("a dry run touches nothing"):
        blank(machine, 2)
        mount = load(machine, 2)
        kura(machine, "write", "-plan", PLAN, "-catalog", CATALOG, "-mount", mount,
             "-volume", volumes[0], "-dry-run", "-quiet")
        # Counted the way the backend counts: files only, minus its own staging
        # directory. A real ext4 cartridge also carries an empty lost+found from
        # mkfs, which List skips because it skips directories — a difference
        # between a real medium and a temp dir that only shows up here.
        left = machine.succeed(
            f"find {mount} -type f -not -path '*/.kura-tmp/*' | sed 's|^{mount}/||'"
        ).split()
        unload(machine)
        assert left == [], f"a dry run left objects on the cartridge: {left}"

    with subtest("every invocation left coverage counters behind"):
        count = int(machine.succeed("ls -1 /var/lib/kura-coverage | wc -l").strip())
        assert count > 1, f"expected counter files in the coverage dir, found {count}"
  '';
}
