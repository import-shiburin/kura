{ lib, stdenv, go }:

stdenv.mkDerivation {
  name = "kura-gofmt-vet";

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      (lib.fileset.fileFilter (f: f.hasExt "go") ../cmd)
      (lib.fileset.fileFilter (f: f.hasExt "go") ../internal)
    ];
  };

  nativeBuildInputs = [ go ];

  buildPhase = ''
    runHook preBuild

    export HOME=$TMPDIR
    export GOCACHE=$TMPDIR/go-cache
    export CGO_ENABLED=0

    unformatted=$(gofmt -l .)
    if [ -n "$unformatted" ]; then
      echo "gofmt would change:" >&2
      echo "$unformatted" >&2
      exit 1
    fi

    go vet ./...

    runHook postBuild
  '';

  installPhase = "touch $out";

  meta.description = "gofmt and go vet, as a flake check";
}
