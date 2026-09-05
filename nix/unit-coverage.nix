# kura's own test suite, run for its coverage counters.
#
# The output is the *raw* counter format rather than a text profile, because
# that is the format the instrumented binary in the VM scenarios also produces.
# Keeping both sides in one format is what lets a single `go tool covdata`
# invocation union unit and end-to-end coverage, instead of needing a second
# pipeline that would drift from the first.
{
  lib,
  stdenv,
  go,
  version ? "dev",
}:

stdenv.mkDerivation {
  pname = "kura-unit-coverage";
  inherit version;

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      (lib.fileset.fileFilter (f: f.hasExt "go") ../cmd)
      (lib.fileset.fileFilter (f: f.hasExt "go") ../internal)
    ];
  };

  nativeBuildInputs = [ go ];

  # No dependencies to fetch, so this builds with the sandbox's network off —
  # which is also the proof that the standard-library-only rule still holds.
  buildPhase = ''
    runHook preBuild

    export HOME=$TMPDIR
    export GOCACHE=$TMPDIR/go-cache
    export GOPATH=$TMPDIR/go
    export CGO_ENABLED=0
    export GOFLAGS=-mod=mod

    mkdir -p $out/raw

    # -coverpkg spans the module so a test in one package credits the code it
    # drives in another; without it the numbers flatter every package by
    # measuring only its own file set.
    go test ./... -cover -coverpkg=kura/... -args -test.gocoverdir=$out/raw

    go tool covdata textfmt -i=$out/raw -o=$out/unit.profile
    go tool covdata percent -i=$out/raw | tee $out/percent.txt

    runHook postBuild
  '';

  installPhase = "true";

  meta.description = "Unit-test coverage counters for kura";
}
