{
  lib,
  buildGoModule,
  runTests ? false,
}:

buildGoModule {
  pname = "covcheck";
  version = "0.1.0";

  src = lib.fileset.toSource {
    root = ../tools/covcheck;
    fileset = lib.fileset.unions [
      ../tools/covcheck/go.mod
      (lib.fileset.fileFilter (f: f.hasExt "go") ../tools/covcheck)
    ];
  };

  vendorHash = null;
  env.CGO_ENABLED = 0;

  # Off by default so building the coverage report does not wait on covcheck's
  # own suite; the flake exposes a separate check that turns it on.
  doCheck = runTests;

  meta = {
    description = "Hold Go coverage to a per-package contract";
    mainProgram = "covcheck";
  };
}
