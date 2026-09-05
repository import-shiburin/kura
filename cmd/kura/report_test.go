package main

import (
	"bytes"
	"strings"
	"testing"

	"kura/internal/catalog"
	"kura/internal/plan"
	"kura/internal/scan"
)

const gb = int64(1e9)

func mkfile(path string, size int64) *scan.File {
	return &scan.File{Path: path, Size: size, Mode: 0o644}
}

func catalogOf(recs ...*catalog.Record) *catalog.Catalog {
	c := catalog.New()
	c.Add(recs...)
	return c
}

func TestGroupKeyIsRootRelative(t *testing.T) {
	roots := []string{"/storage", "/storage_ssd"}
	cases := []struct {
		path  string
		depth int
		want  string
	}{
		{"/storage/Movie/film01.mkv", 2, "/storage/Movie"},
		{"/storage/Anime/Show 01/Season 01/ep01.mkv", 2, "/storage/Anime/Show 01"},
		{"/storage/Anime/Show 01/Season 01/ep01.mkv", 1, "/storage/Anime"},
		{"/storage/readme.txt", 2, "/storage/"},
		{"/storage_ssd/db/x.sql", 1, "/storage_ssd/db"},
		// The longest matching root wins, so a nested root is not swallowed.
		{"/storage/ISOs/disc15.iso", 2, "/storage/ISOs"},
	}
	for _, c := range cases {
		if got := groupKey(c.path, roots, c.depth); got != c.want {
			t.Errorf("groupKey(%q, depth %d) = %q, want %q", c.path, c.depth, got, c.want)
		}
	}
}

func TestTopDirsAggregatesByDirectory(t *testing.T) {
	files := []*scan.File{
		mkfile("/storage/ISOs/a.iso", 40*gb),
		mkfile("/storage/ISOs/b.iso", 30*gb),
		mkfile("/storage/Music/x/1.flac", 5*gb),
	}
	var buf bytes.Buffer
	topDirs(files, []string{"/storage"}, 1, 10, &buf)
	out := buf.String()

	if !strings.Contains(out, "/storage/ISOs") {
		t.Errorf("expected an aggregated /storage/ISOs row, got:\n%s", out)
	}
	if strings.Contains(out, "a.iso") {
		t.Errorf("individual files should not appear in the directory report:\n%s", out)
	}
	if !strings.Contains(out, "70.00 GB") {
		t.Errorf("expected the two ISOs summed to 70 GB, got:\n%s", out)
	}
}

func TestReportRunsOnAnEmptyPlan(t *testing.T) {
	p := &plan.Plan{Format: plan.Format}
	d := new(scan.Delta)
	stats := new(scan.Stats)
	p.Finalize(d, stats, nil)

	var buf bytes.Buffer
	writeReport(&buf, p, d, stats, false)
	if !strings.Contains(buf.String(), "=== VOLUMES ===") {
		t.Errorf("empty plan should still produce a report, got:\n%s", buf.String())
	}
}

// TestReportCallsOutLostRewrites keeps a damaged volume from reading as a busy
// week. The bytes land in the modified count either way, so the report has to
// say which of them are replacing copies that are gone.
func TestReportCallsOutLostRewrites(t *testing.T) {
	d := &scan.Delta{
		Modified:      []*scan.File{mkfile("/storage/x", 100)},
		ModifiedBytes: 100,
		ReplacingLost: 1,
	}
	p := &plan.Plan{Format: plan.Format}
	p.Finalize(d, new(scan.Stats), nil)

	var buf bytes.Buffer
	writeReport(&buf, p, d, new(scan.Stats), false)
	if !strings.Contains(buf.String(), "recorded as lost") {
		t.Errorf("the report did not distinguish damage from ordinary edits:\n%s", buf.String())
	}
}

func TestWriteHistoryRendersTheTimeline(t *testing.T) {
	cat := catalogOf(
		&catalog.Record{Path: "/storage/a", Size: 10, Volume: "LTO001", Gen: 1, DeletedGen: 3},
		&catalog.Record{Path: "/storage/b", Size: 20, Volume: "LTO001", Gen: 1},
		&catalog.Record{Path: "/storage/b", Size: 25, Volume: "LTO002", Gen: 2},
		&catalog.Record{Path: "/storage/c", Size: 30, Volume: "LTO001", Gen: 1, DeletedGen: 2},
		&catalog.Record{Path: "/storage/d", Size: 40, Volume: "LTO003", Gen: 3},
	)
	cat.Header.Timeline = []catalog.GenerationInfo{
		{Gen: 1, Written: "2026-01-05T00:00:00Z", Volumes: []string{"LTO001"}},
		{Gen: 2, Written: "2026-03-02T00:00:00Z", Volumes: []string{"LTO002"}},
		{Gen: 3, Written: "2026-05-11T00:00:00Z", Volumes: []string{"LTO003"}},
	}

	var buf bytes.Buffer
	writeHistory(&buf, cat)
	out := buf.String()

	for _, want := range []string{"2026-01-05T00:00:00Z", "LTO003", "=== HISTORY ==="} {
		if !strings.Contains(out, want) {
			t.Errorf("history output missing %q:\n%s", want, out)
		}
	}

	var stateBuf bytes.Buffer
	if lost := writeStateAt(&stateBuf, cat, 1); lost != 0 {
		t.Errorf("nothing is lost in this catalog, got %d", lost)
	}
	state := stateBuf.String()
	if !strings.Contains(state, "3 files") {
		t.Errorf("generation 1 held 3 files:\n%s", state)
	}
	if !strings.Contains(state, "LTO001") {
		t.Errorf("generation 1 restores from LTO001:\n%s", state)
	}
	if strings.Contains(state, "LTO003") {
		t.Errorf("generation 1 predates LTO003:\n%s", state)
	}
}

// TestStateAtIsHonestAboutHoles is the requirement DESIGN §5.1 states outright:
// a point-in-time query must say "N objects unavailable" rather than quietly
// return a short tree that looks like a complete restore of a smaller archive.
func TestStateAtIsHonestAboutHoles(t *testing.T) {
	cat := catalogOf(
		&catalog.Record{Path: "/storage/fine", Size: 100, Volume: "LTO002", Gen: 1},
		&catalog.Record{Path: "/storage/damaged", Size: 200, Volume: "LTO001", Gen: 1,
			Lost: &catalog.Lost{Gen: 4, Reason: catalog.LostMedia, Detail: "LTO001 unreadable"}},
		&catalog.Record{Path: "/storage/dropped", Size: 300, Volume: "LTO001", Gen: 1,
			Lost: &catalog.Lost{Gen: 5, Reason: catalog.LostReclaimed, Detail: "LTO001 reclaimed"}},
	)

	var buf bytes.Buffer
	lost := writeStateAt(&buf, cat, 1)
	out := buf.String()

	if lost != 2 {
		t.Errorf("expected 2 unavailable objects, got %d", lost)
	}
	// The path is still listed: the tree is reported in full, with the holes
	// named, rather than silently shortened.
	if !strings.Contains(out, "3 files") {
		t.Errorf("all three paths existed at generation 1:\n%s", out)
	}
	if !strings.Contains(out, "UNAVAILABLE") {
		t.Errorf("the holes were not reported:\n%s", out)
	}
	// The two reasons are never collapsed: one was suffered, one was chosen.
	if !strings.Contains(out, "unreadable on their media") {
		t.Errorf("media loss not described:\n%s", out)
	}
	if !strings.Contains(out, "dropped by a reclamation") {
		t.Errorf("reclamation not described:\n%s", out)
	}
}
