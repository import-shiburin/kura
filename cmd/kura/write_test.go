package main

import (
	"strings"
	"testing"

	"kura/internal/catalog"
	"kura/internal/plan"
	"kura/internal/workdir"
)

// catalogWith builds a catalog header for the pairing checks. Records are
// irrelevant here: what a plan is checked against is the archive it belongs to
// and the cycle it is part of, both of which live in the header.
func catalogWith(archive string, gens ...catalog.GenerationInfo) *catalog.Catalog {
	c := catalog.New()
	c.Header.Archive = archive
	c.Header.Timeline = gens
	return c
}

// TestPlanFromAnotherArchiveIsRefused documents the bug the pairing check
// exists for, which was silent and unrecoverable.
//
// `write` took -plan and -catalog as independent arguments and never compared
// them. Given a plan from archive A and the catalog of archive B, it stamped a
// blank cartridge with B's archive ID and A's volume ID, then recorded A's
// files in B's catalog as living on that volume. Afterwards the arrangement is
// entirely self-consistent: `verify` reads the cartridge, finds every object
// the catalog promised, and reports Clean. No check downstream of this one can
// find it, which is why it has to happen before the first object is placed.
func TestPlanFromAnotherArchiveIsRefused(t *testing.T) {
	p := &plan.Plan{Generation: 4, Archive: "aaaa", ID: "plan-a"}
	err := checkPlanCatalog(p, catalogWith("bbbb"), "other.jsonl.gz")
	if err == nil {
		t.Fatal("a plan for archive aaaa was accepted against archive bbbb")
	}
	for _, want := range []string{"aaaa", "bbbb", "other.jsonl.gz"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %q so the operator can tell which pair is wrong: %v", want, err)
		}
	}
}

// TestPlanFromAnotherCycleIsRefused covers the window the archive check cannot
// reach: a plan made against a catalog that had no archive ID yet carries none
// to compare, and that is exactly the first cycle, when two fresh archives look
// alike. The cycle identity closes it — the plan that wrote the generation's
// first volume is named in the timeline, and the rest of the cycle must be it.
func TestPlanFromAnotherCycleIsRefused(t *testing.T) {
	p := &plan.Plan{Generation: 1, ID: "plan-a"} // planned before any archive ID existed
	cat := catalogWith("bbbb", catalog.GenerationInfo{Gen: 1, Plan: "plan-b", Volumes: []string{"BBB001"}})

	err := checkPlanCatalog(p, cat, "other.jsonl.gz")
	if err == nil {
		t.Fatal("a plan was accepted into a generation another plan had started")
	}
	// Naming the volume already written is what turns the error into an
	// instruction: it says which cartridge's plan the operator has to find.
	if !strings.Contains(err.Error(), "BBB001") {
		t.Errorf("the refusal must name what the other plan already wrote: %v", err)
	}
}

// TestResumingTheSameCycleIsAllowed is the ordinary case and the one most at
// risk from an over-strict check: a cycle is one plan and many invocations days
// apart, so every volume after the first meets a timeline entry its own plan
// put there.
func TestResumingTheSameCycleIsAllowed(t *testing.T) {
	p := &plan.Plan{Generation: 1, ID: "plan-a"}
	cat := catalogWith("aaaa", catalog.GenerationInfo{Gen: 1, Plan: "plan-a", Volumes: []string{"AAA001"}})

	if err := checkPlanCatalog(p, cat, "catalog.jsonl.gz"); err != nil {
		t.Fatalf("volume 2 of a cycle was refused its own plan: %v", err)
	}
}

// TestInterruptedFirstWriteIsStillResumable guards a false positive that a
// simpler check would have produced.
//
// The archive ID is persisted before the first object is written, so an
// interrupted first write leaves a catalog that names an archive and holds
// nothing else — no records and no timeline entry. Refusing a plan that
// carries no archive ID whenever the catalog has one would have made that
// cartridge permanently unfinishable, which is the very failure that ordering
// was introduced to fix (see ensureArchiveID, and the last subtest of
// nix/e2e/scenarios/interrupted-write.nix).
func TestInterruptedFirstWriteIsStillResumable(t *testing.T) {
	p := &plan.Plan{Generation: 1, ID: "plan-a"} // planned when the catalog was empty
	cat := catalogWith("aaaa")                   // ID stamped, nothing written yet

	if err := checkPlanCatalog(p, cat, "catalog.jsonl.gz"); err != nil {
		t.Fatalf("an interrupted first write could not be resumed: %v", err)
	}
}

// TestPlanWithoutIdentityIsNotRefused keeps a plan written by an earlier build
// usable. The three identity fields are optional, and an absent one is a check
// that does not run rather than a check that fails.
func TestPlanWithoutIdentityIsNotRefused(t *testing.T) {
	p := &plan.Plan{Generation: 2}
	cat := catalogWith("aaaa", catalog.GenerationInfo{Gen: 2, Plan: "plan-b", Volumes: []string{"AAA001"}})

	if err := checkPlanCatalog(p, cat, "catalog.jsonl.gz"); err != nil {
		t.Fatalf("a plan from a build that recorded no identity was refused: %v", err)
	}
}

// TestOtherGenerationsInTheTimelineAreIgnored: only the cycle being written is
// the plan's business. Every earlier generation was written by some other plan
// and says nothing about this one.
func TestOtherGenerationsInTheTimelineAreIgnored(t *testing.T) {
	p := &plan.Plan{Generation: 3, ID: "plan-c"}
	cat := catalogWith("aaaa",
		catalog.GenerationInfo{Gen: 1, Plan: "plan-a"},
		catalog.GenerationInfo{Gen: 2, Plan: "plan-b"},
	)

	if err := checkPlanCatalog(p, cat, "catalog.jsonl.gz"); err != nil {
		t.Fatalf("a new generation was refused over its predecessors' plans: %v", err)
	}
}

// TestPlanFromThisWorkdirIsNotAnnounced: the ordinary run takes its plan out
// of the workdir it is writing to, and saying so every time would be noise.
func TestPlanFromThisWorkdirIsNotAnnounced(t *testing.T) {
	wd := workdir.At("/var/lib/kura/aaaa")
	if note := planElsewhere(wd, &plan.Plan{Workdir: wd.Path}); note != "" {
		t.Errorf("a plan from its own workdir was announced: %q", note)
	}
}

// TestPlanFromAnotherWorkdirIsAnnounced: -plan can bring in a plan made
// elsewhere, and the workdir is what the command mutates, so the substitution
// is said out loud. The pairing check is not total, and this is the operator's
// only warning when it cannot fire.
func TestPlanFromAnotherWorkdirIsAnnounced(t *testing.T) {
	wd := workdir.At("/var/lib/kura/aaaa")

	note := planElsewhere(wd, &plan.Plan{Workdir: "/var/lib/kura/bbbb"})
	if !strings.Contains(note, wd.Path) || !strings.Contains(note, "/var/lib/kura/bbbb") {
		t.Errorf("the announcement must name both workdirs, got %q", note)
	}
}

// TestPlanWithoutAWorkdirIsNotAnnounced keeps a plan from an earlier build
// quiet: it records no workdir, so there is nothing to disagree with.
func TestPlanWithoutAWorkdirIsNotAnnounced(t *testing.T) {
	if note := planElsewhere(workdir.At("/var/lib/kura/aaaa"), &plan.Plan{}); note != "" {
		t.Errorf("a plan that records no workdir was announced: %q", note)
	}
}

// TestRecordGenerationBackfillsThePlanOfAnOlderCycle: a generation begun by a
// build that recorded no plan would otherwise stay unchecked for the whole
// cycle. The first volume written by a build that does record one claims it, so
// the check starts working at the next cartridge instead of the next cycle.
func TestRecordGenerationBackfillsThePlanOfAnOlderCycle(t *testing.T) {
	cat := catalogWith("aaaa", catalog.GenerationInfo{Gen: 1, Volumes: []string{"AAA001"}})

	recordGeneration(cat, 1, "AAA002", "plan-a")

	g := cat.Header.Timeline[0]
	if g.Plan != "plan-a" {
		t.Errorf("the plan should have been backfilled onto the open cycle, got %q", g.Plan)
	}
	if len(g.Volumes) != 2 || g.Volumes[1] != "AAA002" {
		t.Errorf("the new volume should have joined the cycle, got %v", g.Volumes)
	}
}

// TestRecordGenerationKeepsTheCyclesOriginalPlan: backfilling must not become
// overwriting, or a mismatched plan would rewrite the identity it is meant to
// be checked against and every later volume would agree with it.
func TestRecordGenerationKeepsTheCyclesOriginalPlan(t *testing.T) {
	cat := catalogWith("aaaa", catalog.GenerationInfo{Gen: 1, Plan: "plan-a", Volumes: []string{"AAA001"}})

	recordGeneration(cat, 1, "AAA002", "plan-b")

	if got := cat.Header.Timeline[0].Plan; got != "plan-a" {
		t.Errorf("the cycle's plan was overwritten with %q", got)
	}
}
