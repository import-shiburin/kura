# Coverage and end-to-end testing.
#
# Two rules shape everything here.
#
# One coverage bus. Unit tests and the instrumented binary in the VM scenarios
# both emit Go's raw counter format, so a single `go tool covdata` invocation
# unions them. There is no second pipeline to keep in step, and no format
# conversion that can quietly lose a package.
#
# Nix owns the VM side. Each scenario is its own derivation, so they run in
# parallel, cache when nothing changed, and fail one at a time.

SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c

GO ?= go
export CGO_ENABLED := 0

COVDIR   := coverage
RAW      := $(COVDIR)/raw
PROFILE  := $(COVDIR)/merged.profile
COVCHECK := $(COVDIR)/bin/covcheck
# The merged unit+e2e profile when one exists, otherwise the unit-only one.
# Reports should describe the fullest picture available without insisting the
# VM suite has been run.
BEST      = $(if $(wildcard $(COVDIR)/all.profile),$(COVDIR)/all.profile,$(PROFILE))
SYSTEM   := $(shell nix eval --raw --impure --expr 'builtins.currentSystem' 2>/dev/null || echo x86_64-linux)

.PHONY: help
help:
	@sed -n 's/^##//p' $(MAKEFILE_LIST) | column -t -s ':'

## test:run the unit tests
test:
	$(GO) test ./...
	cd tools/covcheck && $(GO) test ./...

## fmt:format everything
fmt:
	gofmt -w .
	cd tools/covcheck && gofmt -w .
	command -v nixfmt >/dev/null && nixfmt flake.nix nix/**/*.nix nix/*.nix || true

## vet:gofmt and go vet, the pre-commit gate from CLAUDE.md
vet:
	@test -z "$$(gofmt -l . | tee /dev/stderr)" || { echo "gofmt would change files"; exit 1; }
	$(GO) vet ./...
	cd tools/covcheck && $(GO) vet ./...

# ---------------------------------------------------------------- coverage --

$(COVCHECK): $(wildcard tools/covcheck/*.go) tools/covcheck/go.mod
	@mkdir -p $(dir $@)
	cd tools/covcheck && $(GO) build -o $(CURDIR)/$@ .

## cover:unit coverage, reported (the inner loop; does not gate)
#
# Report only, deliberately. The floors in .covcheck.json describe the whole
# suite, and most of what reaches cmd/kura comes from the VM scenarios, so
# gating a unit-only profile against them would fail every time and teach
# everyone to ignore the gate. Use cover-gate or cover-all for a verdict.
cover: $(COVCHECK)
	@rm -rf $(RAW) && mkdir -p $(RAW)
	$(GO) test ./... -cover -coverpkg=kura/... -args -test.gocoverdir=$(CURDIR)/$(RAW)
	$(GO) tool covdata textfmt -i=$(RAW) -o=$(PROFILE)
	@echo
	@$(COVCHECK) report -i $(PROFILE) -floors=false
	@echo
	@echo "note: unit coverage only, so no floors are shown. For a verdict run"
	@echo "      'make cover-gate' (merges the e2e scenarios in) or 'make cover-all'."

## cover-gate:merge unit and e2e coverage, then fail if anything is under its floor
cover-gate: cover-merge
	@$(COVCHECK) gate -i $(COVDIR)/all.profile

## cover-todo:the biggest uncovered blocks, worst first
cover-todo: $(COVCHECK)
	@$(COVCHECK) uncovered -i $(BEST) -n 40

## cover-merge:merge local unit coverage with the scenarios' coverage
cover-merge: cover
	@out=$$(nix build .#e2e --no-link --print-out-paths); \
	 dirs=$(RAW); \
	 for d in "$$out"/*/kura-coverage; do dirs="$$dirs,$$d"; done; \
	 $(GO) tool covdata textfmt -i="$$dirs" -o=$(COVDIR)/all.profile
	@echo
	@$(COVCHECK) report -i $(COVDIR)/all.profile

## cover-ratchet:raise every floor to what the suite reaches now
#
# Reads the merged profile when there is one. Ratcheting from unit coverage
# alone would never raise a floor, since the floors already describe unit+e2e.
cover-ratchet: $(COVCHECK)
	@$(COVCHECK) ratchet -i $(BEST)

## cover-html:browsable HTML report
cover-html:
	$(GO) tool cover -html=$(BEST) -o $(COVDIR)/coverage.html
	@echo "wrote $(COVDIR)/coverage.html"

## cover-lcov:lcov tracefile, plus genhtml output when lcov is installed
cover-lcov: $(COVCHECK)
	@$(COVCHECK) lcov -i $(BEST) -o $(COVDIR)/coverage.lcov
	@command -v genhtml >/dev/null && genhtml -q -o $(COVDIR)/lcov-html $(COVDIR)/coverage.lcov \
		&& echo "wrote $(COVDIR)/lcov-html/index.html" \
		|| echo "wrote $(COVDIR)/coverage.lcov (install lcov for genhtml)"

## cover-tui:browse the profile in the terminal (needs gocovsh)
cover-tui:
	gocovsh --profile $(BEST)

# --------------------------------------------------------------------- e2e --

## e2e-list:the scenarios that exist
e2e-list:
	@nix eval --raw .#packages.$(SYSTEM).e2e.name >/dev/null 2>&1 || true
	@ls nix/e2e/scenarios/*.nix | xargs -n1 basename | sed 's/\.nix$$//' | sed 's/^/  /'

## e2e:run every VM scenario
e2e:
	nix build .#e2e -L --no-link

## e2e-1:run one scenario, e.g. make e2e-1 S=bitrot
e2e-1:
	@test -n "$(S)" || { echo "usage: make e2e-1 S=<scenario>"; exit 2; }
	nix build .#checks.$(SYSTEM).e2e-$(S) -L --no-link

## e2e-shell:boot one scenario's VM interactively, e.g. make e2e-shell S=bitrot
e2e-shell:
	@test -n "$(S)" || { echo "usage: make e2e-shell S=<scenario>"; exit 2; }
	nix run .#checks.$(SYSTEM).e2e-$(S).driverInteractive

# ------------------------------------------------------- unit + e2e, gated --

## cover-all:merge unit and every scenario's coverage, then gate (runs the VMs)
cover-all:
	@mkdir -p $(COVDIR)
	nix build .#coverage -L --no-link --print-out-paths | tee $(COVDIR)/.nix-out
	@cat "$$(cat $(COVDIR)/.nix-out)/report.txt"

## check:everything the flake knows how to check
check:
	nix flake check -L

## clean:remove coverage output
clean:
	rm -rf $(COVDIR) result result-*
