# Everything, merged: unit coverage plus every VM scenario, in one profile.
#
# This is the derivation that makes the two halves of the testing story a
# single number. It depends on every scenario, so building it runs the whole
# VM suite; `make cover` is the fast unit-only path for the inner loop.
{
  lib,
  stdenv,
  go,
  covcheck,
  unit-coverage,
  scenarios,
}:

let
  # Each scenario derivation leaves its counters at $out/kura-coverage, the
  # directory copy_from_vm produced.
  scenarioDirs = lib.mapAttrsToList (name: drv: "${drv}/kura-coverage") scenarios;

  # `go tool covdata` takes one comma-separated -i list. Scenario count grows;
  # the list is generated rather than maintained.
  inputDirs = lib.concatStringsSep "," ([ "${unit-coverage}/raw" ] ++ scenarioDirs);

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../.covcheck.json
      (lib.fileset.fileFilter (f: f.hasExt "go") ../cmd)
      (lib.fileset.fileFilter (f: f.hasExt "go") ../internal)
    ];
  };
in
stdenv.mkDerivation {
  name = "kura-coverage";

  nativeBuildInputs = [
    go
    covcheck
  ];

  dontUnpack = true;

  buildPhase = ''
    runHook preBuild

    export HOME=$TMPDIR
    export GOCACHE=$TMPDIR/go-cache
    export CGO_ENABLED=0

    # go tool cover -html resolves the import paths in a profile through the
    # module, so it has to run with the source tree present and writable.
    cp -r ${src} $TMPDIR/src
    chmod -R u+w $TMPDIR/src

    mkdir -p $out

    echo "merging coverage from:"
    tr ',' '\n' <<< "${inputDirs}" | sed 's/^/  /'

    go tool covdata textfmt -i="${inputDirs}" -o=$out/merged.profile
    go tool covdata percent -i="${inputDirs}" | tee $out/percent.txt

    cd $TMPDIR/src

    go tool cover -html=$out/merged.profile -o=$out/coverage.html
    go tool cover -func=$out/merged.profile > $out/func.txt

    covcheck report -i $out/merged.profile -root . > $out/report.txt
    covcheck report -i $out/merged.profile -root . -format markdown > $out/report.md
    covcheck report -i $out/merged.profile -root . -format json > $out/report.json
    covcheck lcov   -i $out/merged.profile -root . -o $out/coverage.lcov
    covcheck uncovered -i $out/merged.profile -root . -n 0 > $out/uncovered.txt

    cat $out/report.txt

    runHook postBuild
  '';

  # The gate is the last step, so the artifacts above are written even when it
  # fails: a build that only says "coverage too low" without saying where is
  # the reason people stop running coverage gates.
  installPhase = ''
    runHook preInstall
    cd $TMPDIR/src
    covcheck gate -i $out/merged.profile -root .
    runHook postInstall
  '';

  meta.description = "Merged unit and end-to-end coverage for kura";
}
