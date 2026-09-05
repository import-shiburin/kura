package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func parse(t *testing.T, text string) *Profile {
	t.Helper()
	p, err := ParseProfile(strings.NewReader(text))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return p
}

func TestParseKeepsTheProfileFieldsIntact(t *testing.T) {
	p := parse(t, "mode: set\nkura/internal/plan/pack.go:12.34,56.78 9 3\n")
	if p.Mode != "set" {
		t.Errorf("mode = %q, want set", p.Mode)
	}
	blocks := p.Blocks()
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	got := blocks[0]
	want := Block{File: "kura/internal/plan/pack.go", StartLine: 12, StartCol: 34, EndLine: 56, EndCol: 78, NumStmt: 9, Count: 3}
	if got != want {
		t.Errorf("block = %+v, want %+v", got, want)
	}
}

// A merge that summed counts would still answer "covered?" correctly but would
// double the denominator when the same block arrives from two pods, which is
// exactly what happens once e2e coverage joins unit coverage.
func TestMergingTheSameBlockTwiceDoesNotDoubleCountIt(t *testing.T) {
	a := parse(t, "mode: set\nkura/a/x.go:1.1,2.2 4 1\n")
	b := parse(t, "mode: set\nkura/a/x.go:1.1,2.2 4 1\n")
	a.Merge(b)

	s := a.Summarise(&Excluder{})
	if s.Total.Total != 4 || s.Total.Covered != 4 {
		t.Errorf("tally = %d/%d, want 4/4", s.Total.Covered, s.Total.Total)
	}
}

// The whole reason e2e coverage is worth collecting: a block no unit test
// reaches is covered by the run of the real binary, and the merge has to see
// that rather than keeping whichever pod was read last.
func TestMergeTakesCoverageFromEitherSource(t *testing.T) {
	unit := parse(t, "mode: set\nkura/cmd/kura/main.go:1.1,2.2 3 0\nkura/cmd/kura/main.go:5.1,6.2 2 1\n")
	e2e := parse(t, "mode: set\nkura/cmd/kura/main.go:1.1,2.2 3 1\nkura/cmd/kura/main.go:5.1,6.2 2 0\n")
	unit.Merge(e2e)

	s := unit.Summarise(&Excluder{})
	if s.Total.Covered != 5 {
		t.Errorf("covered = %d, want 5 (both blocks, one from each source)", s.Total.Covered)
	}
}

func TestSummariseGroupsByPackageDirectory(t *testing.T) {
	p := parse(t, `mode: set
kura/internal/plan/pack.go:1.1,2.2 4 1
kura/internal/plan/group.go:1.1,2.2 6 0
kura/internal/scan/scan.go:1.1,2.2 5 1
`)
	s := p.Summarise(&Excluder{})

	if got := s.Packages["kura/internal/plan"]; got.Covered != 4 || got.Total != 10 {
		t.Errorf("plan = %d/%d, want 4/10", got.Covered, got.Total)
	}
	if got := s.Packages["kura/internal/scan"]; got.Covered != 5 || got.Total != 5 {
		t.Errorf("scan = %d/%d, want 5/5", got.Covered, got.Total)
	}
	if got := s.Total.Percent(); got != 60 {
		t.Errorf("total = %.1f%%, want 60.0%%", got)
	}
}

// An exclusion has to leave the denominator, not just the numerator. Excluding
// from the numerator alone would make excluding a file *lower* the percentage,
// which is the opposite of what anyone writing the config expects.
func TestExclusionRemovesAFileFromBothSidesOfTheRatio(t *testing.T) {
	p := parse(t, `mode: set
kura/internal/plan/pack.go:1.1,2.2 5 1
kura/internal/plan/generated.go:1.1,2.2 95 0
`)
	cfg := &Config{Exclude: []Exclusion{{Pattern: "kura/internal/plan/generated.go", Reason: "generated"}}}
	ex, err := cfg.Excluder()
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Summarise(ex).Total.Percent(); got != 100 {
		t.Errorf("total = %.1f%%, want 100.0%% once the generated file is out", got)
	}
}

func TestExcluderPatternForms(t *testing.T) {
	cfg := &Config{Exclude: []Exclusion{
		{Pattern: "kura/internal/scan/hash.go", Reason: "exact"},
		{Pattern: "kura/vendored/...", Reason: "subtree"},
		{Pattern: "*_string.go", Reason: "base-name glob"},
		{Pattern: "kura/internal/*/mock.go", Reason: "path glob"},
	}}
	ex, err := cfg.Excluder()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		file string
		want bool
	}{
		{"kura/internal/scan/hash.go", true},
		{"kura/internal/scan/hasher.go", false},
		{"kura/vendored/deep/thing.go", true},
		{"kura/internal/plan/status_string.go", true},
		{"kura/internal/plan/mock.go", true},
		{"kura/internal/plan/pack.go", false},
	} {
		if got := ex.Excluded(tc.file); got != tc.want {
			t.Errorf("Excluded(%q) = %v, want %v", tc.file, got, tc.want)
		}
	}
}

// A config is a contract other people read. An exclusion whose justification
// was never written down is indistinguishable from one added to make a build
// go green, so loading refuses it.
func TestConfigRefusesAnExclusionWithNoReason(t *testing.T) {
	dir := t.TempDir()
	pth := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(pth, []byte(`{"total":90,"exclude":[{"pattern":"kura/x/y.go"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(pth); err == nil {
		t.Fatal("want an error for an exclusion with no reason")
	}
}

func TestConfigRefusesUnknownFields(t *testing.T) {
	dir := t.TempDir()
	pth := filepath.Join(dir, "cfg.json")
	// A typo in a threshold key silently means "no floor at all", which is the
	// one failure mode a coverage gate must never have.
	if err := os.WriteFile(pth, []byte(`{"total":90,"packagez":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(pth); err == nil {
		t.Fatal("want an error for an unknown config field")
	}
}

func TestGateFailsThePackageThatSlipped(t *testing.T) {
	p := parse(t, `mode: set
kura/internal/plan/pack.go:1.1,2.2 5 1
kura/internal/plan/pack.go:3.1,4.2 5 0
kura/internal/scan/scan.go:1.1,2.2 10 1
`)
	cfg := &Config{
		Total:    70,
		Default:  0,
		Packages: map[string]float64{"kura/internal/plan": 90, "kura/internal/scan": 100},
	}
	rep := BuildReport(p.Summarise(&Excluder{}), cfg)

	if rep.Pass {
		t.Fatal("report passed; plan is 50% against a floor of 90%")
	}
	fails := rep.Failures()
	if len(fails) != 1 || fails[0].Name != "kura/internal/plan" {
		t.Fatalf("failures = %+v, want only kura/internal/plan", fails)
	}
}

// The ratchet is the mechanism that turns a target into progress, so the
// direction matters more than the arithmetic: it may raise a floor and must
// never lower one.
func TestRatchetRaisesFloorsAndNeverLowersThem(t *testing.T) {
	p := parse(t, `mode: set
kura/internal/plan/pack.go:1.1,2.2 9 1
kura/internal/plan/pack.go:3.1,4.2 1 0
kura/internal/scan/scan.go:1.1,2.2 5 0
`)
	cfg := &Config{
		Total:    0,
		Packages: map[string]float64{"kura/internal/scan": 80},
	}
	cfg.Ratchet(p.Summarise(&Excluder{}), 0.1)

	if got := cfg.Packages["kura/internal/plan"]; got != 90 {
		t.Errorf("plan floor = %.1f, want 90.0", got)
	}
	if got := cfg.Packages["kura/internal/scan"]; got != 80 {
		t.Errorf("scan floor = %.1f, want it left at 80.0 rather than dropped to 0", got)
	}
}

// Rounding up by even a hundredth of a point sets a floor above what was
// measured, and the very next run fails against a number it just produced.
func TestRatchetRoundsDownSoTheFloorItSetsStillPasses(t *testing.T) {
	p := parse(t, "mode: set\nkura/a/x.go:1.1,2.2 1 1\nkura/a/x.go:3.1,4.2 2 0\n")
	cfg := DefaultConfig()
	s := p.Summarise(&Excluder{})
	cfg.Ratchet(s, 0.1)

	if got := cfg.Packages["kura/a"]; got > s.Packages["kura/a"].Percent() {
		t.Errorf("floor %.4f exceeds measured %.4f", got, s.Packages["kura/a"].Percent())
	}
	if !BuildReport(s, cfg).Pass {
		t.Error("the floors a ratchet just set do not pass the run they came from")
	}
}

// The config is committed and hand-edited, so the numbers a ratchet writes
// have to be readable. Computing the floor as float64(int(v/r))*r produces
// 84.10000000000001, which is correct and looks like a mistake.
func TestRatchetWritesCleanDecimals(t *testing.T) {
	for _, tc := range []struct {
		covered, total int
		want           string
	}{
		{1977, 2349, "84.1"}, // 84.1634...
		{146, 180, "81.1"},   // 81.1111...
		{5, 5, "100"},        // exactly 100
		{317, 337, "94"},     // 94.0652...
	} {
		p := NewProfile()
		p.Add(Block{File: "kura/a/x.go", StartLine: 1, EndLine: 1, NumStmt: tc.covered, Count: 1})
		if tc.total > tc.covered {
			p.Add(Block{File: "kura/a/x.go", StartLine: 2, EndLine: 2, NumStmt: tc.total - tc.covered, Count: 0})
		}
		cfg := DefaultConfig()
		cfg.Ratchet(p.Summarise(&Excluder{}), 0.1)

		data, err := json.Marshal(cfg.Packages["kura/a"])
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != tc.want {
			t.Errorf("%d/%d ratcheted to %s, want %s", tc.covered, tc.total, data, tc.want)
		}
	}
}

// A package of nothing but declarations has no statements to cover. Calling
// that 0% would keep a 100% target permanently out of reach for no reason.
func TestAPackageWithNoStatementsCountsAsComplete(t *testing.T) {
	if got := (Tally{}).Percent(); got != 100 {
		t.Errorf("empty tally = %.1f%%, want 100.0%%", got)
	}
}

func TestGapsAreOrderedBiggestFirst(t *testing.T) {
	p := parse(t, `mode: set
kura/a/x.go:10.1,11.2 2 0
kura/a/x.go:20.1,21.2 30 0
kura/a/x.go:30.1,31.2 7 1
`)
	gaps := Gaps(p, &Excluder{}, t.TempDir(), "kura")
	if len(gaps) != 2 {
		t.Fatalf("got %d gaps, want 2 (the covered block is not a gap)", len(gaps))
	}
	if gaps[0].NumStmt != 30 || gaps[1].NumStmt != 2 {
		t.Errorf("order = %d,%d, want 30,2", gaps[0].NumStmt, gaps[1].NumStmt)
	}
}

func TestGapsCarryTheSourceLineWhenTheTreeIsPresent(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "internal", "plan"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := "package plan\n\nfunc unreached() {\n\tpanic(\"no\")\n}\n"
	if err := os.WriteFile(filepath.Join(root, "internal", "plan", "pack.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	p := parse(t, "mode: set\nkura/internal/plan/pack.go:3.1,5.2 1 0\n")

	gaps := Gaps(p, &Excluder{}, root, "kura")
	if len(gaps) != 1 {
		t.Fatalf("got %d gaps, want 1", len(gaps))
	}
	if gaps[0].Source != "func unreached() {" {
		t.Errorf("source = %q, want the declaration line", gaps[0].Source)
	}
}

// Profiles name files by import path so they survive being moved between
// machines; anything that reads source has to undo that, and getting it wrong
// silently produces an lcov file genhtml renders as an empty tree.
func TestDiskPathStripsTheModulePrefix(t *testing.T) {
	got := DiskPath("/repo", "kura", "kura/internal/plan/pack.go")
	want := filepath.Join("/repo", "internal", "plan", "pack.go")
	if got != want {
		t.Errorf("DiskPath = %q, want %q", got, want)
	}
	// A file from some other module must not be mangled into this one's tree.
	other := DiskPath("/repo", "kura", "example.com/dep/z.go")
	if other != filepath.Join("/repo", "example.com", "dep", "z.go") {
		t.Errorf("unexpected mapping for a foreign module: %q", other)
	}
}

func TestLCOVReportsHitAndMissedLines(t *testing.T) {
	p := parse(t, "mode: set\nkura/a/x.go:1.1,2.2 1 1\nkura/a/x.go:5.1,5.9 1 0\n")
	var sb strings.Builder
	if err := WriteLCOV(&sb, p, &Excluder{}, "/repo", "kura"); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{"SF:" + filepath.Join("/repo", "a", "x.go"), "DA:1,1", "DA:2,1", "DA:5,0", "LF:3", "LH:2", "end_of_record"} {
		if !strings.Contains(out, want) {
			t.Errorf("lcov missing %q in:\n%s", want, out)
		}
	}
}

func TestProfileRoundTripsThroughText(t *testing.T) {
	in := "mode: atomic\nkura/a/x.go:1.1,2.2 3 4\nkura/b/y.go:5.6,7.8 9 0\n"
	p := parse(t, in)
	var sb strings.Builder
	if err := p.Write(&sb); err != nil {
		t.Fatal(err)
	}
	if sb.String() != in {
		t.Errorf("round trip changed the profile:\ngot  %q\nwant %q", sb.String(), in)
	}
}

func TestExpandInputsPullsProfilesOutOfADirectory(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.profile", "b.out", "c.cov", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("mode: set\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := expandInputs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %v, want the three coverage files and not notes.txt", got)
	}
}

func TestModulePathComesFromGoMod(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("// a comment\nmodule example.com/thing\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := modulePath(dir); got != "example.com/thing" {
		t.Errorf("modulePath = %q, want example.com/thing", got)
	}
}

func TestParseRejectsAMalformedRecord(t *testing.T) {
	for _, bad := range []string{
		"kura/a/x.go 1 1",
		"kura/a/x.go:1.1 1 1",
		"kura/a/x.go:1.1,2.2 x 1",
		"kura/a/x.go:oops,2.2 1 1",
	} {
		if _, err := ParseProfile(strings.NewReader("mode: set\n" + bad + "\n")); err == nil {
			t.Errorf("parsed %q without error", bad)
		}
	}
}
