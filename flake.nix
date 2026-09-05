{
  description = "kura - plan and inspect an offline multi-volume archive";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f system nixpkgs.legacyPackages.${system});

      version = "0.1.0-${self.shortRev or self.dirtyShortRev or "dirty"}";
    in
    {
      packages = forAllSystems (
        system: pkgs:
        let
          # Not named `e2e`: the rec block below defines an `e2e` attribute of
          # its own, and inside a rec that name would resolve to the derivation
          # rather than to the suite.
          suite = import ./nix/e2e {
            inherit pkgs;
            kura = self.packages.${system}.kura-cover;
          };
        in
        rec {
          default = kura;

          kura = pkgs.callPackage ./nix/kura.nix { inherit version; };

          # The instrumented twin. Same source, same flags, plus Go's binary
          # coverage counters.
          kura-cover = pkgs.callPackage ./nix/kura.nix {
            inherit version;
            coverage = true;
          };

          covcheck = pkgs.callPackage ./nix/covcheck.nix { };

          # Unit coverage, in the raw counter format so it merges with the
          # e2e runs rather than needing a second, incompatible pipeline.
          unit-coverage = pkgs.callPackage ./nix/unit-coverage.nix { inherit version; };

          # Everything — unit plus every scenario — reduced to one profile,
          # one HTML report, one lcov file and one gate verdict.
          coverage = pkgs.callPackage ./nix/coverage.nix {
            inherit (self.packages.${system}) covcheck unit-coverage;
            inherit (suite) scenarios;
          };

          # `nix build .#e2e` runs the whole VM suite, and collects each
          # scenario's coverage under one path so the merge can glob for them.
          e2e = pkgs.linkFarm "kura-e2e-all" (
            pkgs.lib.mapAttrsToList (name: drv: {
              inherit name;
              path = drv;
            }) suite.scenarios
          );
        }
      );

      # `nix flake check` runs the unit tests, the coverage gate, and every VM
      # scenario. Each scenario is its own derivation, so Nix runs them in
      # parallel up to max-jobs, caches the ones whose inputs did not change,
      # and attributes a failure to exactly one scenario.
      checks = forAllSystems (
        system: pkgs:
        let
          e2e = import ./nix/e2e {
            inherit pkgs;
            kura = self.packages.${system}.kura-cover;
          };
        in
        e2e.scenarios
        // {
          unit = self.packages.${system}.unit-coverage;
          covcheck = pkgs.callPackage ./nix/covcheck.nix { runTests = true; };
          coverage-gate = self.packages.${system}.coverage;
          gofmt = pkgs.callPackage ./nix/gofmt-check.nix { };
        }
      );

      devShells = forAllSystems (
        system: pkgs: {
          default = pkgs.mkShell {
            packages = [
              pkgs.go
              pkgs.gopls
              pkgs.go-tools # staticcheck
              self.packages.${system}.covcheck

              # Coverage viewers. All OSS, all local: nothing here uploads a
              # profile to a service, which is the point.
              pkgs.lcov # genhtml, the standard OSS HTML report
              pkgs.gocovsh # terminal browser over a profile
              pkgs.go-cover-treemap # one SVG showing where the gaps are

              pkgs.gotestsum # readable test output in CI
              pkgs.qemu_kvm # what the VM scenarios run on
              pkgs.jq
            ];

            shellHook = ''
              export CGO_ENABLED=0
              echo "kura dev shell"
              echo "  make cover        unit coverage, merged and reported"
              echo "  make cover-html   open a browsable report"
              echo "  make cover-todo   the biggest uncovered blocks"
              echo "  make e2e          every VM scenario"
              echo "  make e2e-list     what scenarios exist"
              echo "  make cover-all    unit + e2e, merged and gated"
            '';
          };
        }
      );

      formatter = forAllSystems (system: pkgs: pkgs.nixfmt-rfc-style);
    };
}
