# The kura binary, in two flavours.
#
# `coverage = true` builds the same program with Go's binary coverage
# instrumentation (`go build -cover`). That is the whole mechanism behind
# end-to-end coverage: an instrumented binary writes the same raw counter files
# that `go test -cover` writes, so a run inside a VM and a unit test land in one
# merged profile with no bespoke plumbing.
#
# It flushes on os.Exit as well as on a normal return, which matters here
# because every kura subcommand ends in os.Exit — including the argument-error
# paths that unit tests cannot reach at all.
{
  lib,
  buildGoModule,
  go,
  coverage ? false,
  version ? "dev",
}:

buildGoModule {
  pname = if coverage then "kura-cover" else "kura";
  inherit version;

  src = lib.fileset.toSource {
    root = ../.;
    # Only the things that change the binary. Documentation churn should not
    # invalidate a build, and it should certainly not re-run the VM suite.
    fileset = lib.fileset.unions [
      ../go.mod
      (lib.fileset.fileFilter (f: f.hasExt "go") ../cmd)
      (lib.fileset.fileFilter (f: f.hasExt "go") ../internal)
    ];
  };

  # No dependencies, and the rule that keeps it that way is in CLAUDE.md: the
  # binary is copied onto a backup host and run there. A non-null vendorHash
  # here would mean somebody added one.
  vendorHash = null;

  inherit go;

  # Same reason: the host it lands on gets a static binary, not one that needs
  # a matching libc.
  env.CGO_ENABLED = 0;

  # Appended in the shell rather than set through `env.GOFLAGS` or the
  # builder's own GOFLAGS argument. buildGoModule composes GOFLAGS itself (it
  # adds -mod=vendor and -trimpath), so an env-level setting is silently
  # discarded — which yields a binary that looks instrumented and emits no
  # counters — while passing the argument collides with that same composed
  # value. Extending the exported variable is the one place both agree.
  #
  # -coverpkg spans the whole module rather than just main, so driving the CLI
  # credits the packages underneath it.
  preBuild = lib.optionalString coverage ''
    export GOFLAGS="$GOFLAGS -cover -coverpkg=kura/..."
  '';

  ldflags = [
    "-s"
    "-w"
    "-X main.Version=${version}"
  ];

  # Unit tests run as their own flake check, where a failure is attributable.
  # Running them here too would only make every VM scenario wait on them.
  doCheck = false;

  meta = {
    description = "Plan and inspect an offline multi-volume archive";
    mainProgram = "kura";
  };
}
