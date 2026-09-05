package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Row is one line of a report: a package or a file measured against its floor.
type Row struct {
	Name      string  `json:"name"`
	Covered   int     `json:"covered"`
	Total     int     `json:"total"`
	Percent   float64 `json:"percent"`
	Threshold float64 `json:"threshold"`
	Pass      bool    `json:"pass"`
}

// Report is the whole comparison of a summary against a config.
type Report struct {
	Total    Row   `json:"total"`
	Packages []Row `json:"packages"`
	Pass     bool  `json:"pass"`
}

// tolerance absorbs the float error of turning a ratio into a percentage.
//
// Without it a package can measure 93.99999999% against a floor of 94.0 that a
// previous ratchet set from the identical block counts, and fail on nothing.
const tolerance = 1e-9

func BuildReport(s *Summary, cfg *Config) *Report {
	r := &Report{Pass: true}

	r.Total = Row{
		Name:      "total",
		Covered:   s.Total.Covered,
		Total:     s.Total.Total,
		Percent:   s.Total.Percent(),
		Threshold: cfg.Total,
	}
	r.Total.Pass = r.Total.Percent >= r.Total.Threshold-tolerance
	r.Pass = r.Pass && r.Total.Pass

	for _, pkg := range s.PackageNames() {
		t := s.Packages[pkg]
		row := Row{
			Name:      pkg,
			Covered:   t.Covered,
			Total:     t.Total,
			Percent:   t.Percent(),
			Threshold: cfg.Threshold(pkg),
		}
		row.Pass = row.Percent >= row.Threshold-tolerance
		r.Pass = r.Pass && row.Pass
		r.Packages = append(r.Packages, row)
	}
	return r
}

// Failures lists the rows that came in under their floor.
func (r *Report) Failures() []Row {
	var out []Row
	for _, row := range r.Packages {
		if !row.Pass {
			out = append(out, row)
		}
	}
	if !r.Total.Pass {
		out = append(out, r.Total)
	}
	return out
}

func (r *Report) WriteText(w io.Writer) { r.writeText(w, true) }

// WriteTextNoFloors prints the same table without the floor column.
//
// For a profile that is only part of the picture — unit coverage on its own,
// when the floors describe unit plus end-to-end. Showing "BELOW FLOOR" there
// would be a verdict on a comparison nobody is making, and a gate people learn
// to ignore is worse than no gate.
func (r *Report) WriteTextNoFloors(w io.Writer) { r.writeText(w, false) }

func (r *Report) writeText(w io.Writer, floors bool) {
	bw := bufio.NewWriter(w)
	defer bw.Flush()

	width := len("PACKAGE")
	for _, row := range r.Packages {
		if len(row.Name) > width {
			width = len(row.Name)
		}
	}

	line := func(name string, row Row) {
		fmt.Fprintf(bw, "%-*s  %4d/%-4d  %7.2f%%", width, name, row.Covered, row.Total, row.Percent)
		if floors {
			fmt.Fprintf(bw, "  %7.2f%%   %s", row.Threshold, mark(row.Pass))
		}
		fmt.Fprintln(bw)
	}

	fmt.Fprintf(bw, "%-*s  %9s  %7s", width, "PACKAGE", "STMTS", "COVER")
	if floors {
		fmt.Fprintf(bw, "  %7s   %s", "FLOOR", "")
	}
	fmt.Fprintln(bw)

	for _, row := range r.Packages {
		line(row.Name, row)
	}
	line("TOTAL", r.Total)
}

func mark(pass bool) string {
	if pass {
		return "ok"
	}
	return "BELOW FLOOR"
}

// WriteMarkdown emits a table for a CI job summary.
func (r *Report) WriteMarkdown(w io.Writer) {
	bw := bufio.NewWriter(w)
	defer bw.Flush()

	fmt.Fprintf(bw, "| Package | Statements | Coverage | Floor | |\n")
	fmt.Fprintf(bw, "|---|---:|---:|---:|:-:|\n")
	for _, row := range r.Packages {
		fmt.Fprintf(bw, "| `%s` | %d/%d | %.2f%% | %.2f%% | %s |\n",
			row.Name, row.Covered, row.Total, row.Percent, row.Threshold, tick(row.Pass))
	}
	fmt.Fprintf(bw, "| **total** | **%d/%d** | **%.2f%%** | **%.2f%%** | %s |\n",
		r.Total.Covered, r.Total.Total, r.Total.Percent, r.Total.Threshold, tick(r.Total.Pass))
}

func tick(pass bool) string {
	if pass {
		return "✅"
	}
	return "❌"
}

func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// Gap is a run of uncovered statements, with the source line that starts it.
type Gap struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Col     int    `json:"col"`
	NumStmt int    `json:"statements"`
	Source  string `json:"source,omitempty"`
}

// Gaps lists what is not covered, biggest first.
//
// This is the subcommand that actually moves the number: a percentage says how
// far there is to go, and this says what to write next. Sorting by statement
// count puts the cheapest wins at the top, because one untested 40-statement
// branch is worth more than forty one-statement error returns.
func Gaps(p *Profile, ex *Excluder, root, module string) []Gap {
	var out []Gap
	for _, b := range p.Blocks() {
		if b.Covered() || ex.Excluded(b.File) {
			continue
		}
		out = append(out, Gap{
			File:    b.File,
			Line:    b.StartLine,
			Col:     b.StartCol,
			NumStmt: b.NumStmt,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].NumStmt != out[j].NumStmt {
			return out[i].NumStmt > out[j].NumStmt
		}
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	annotate(out, root, module)
	return out
}

// annotate attaches the first source line of each gap.
//
// Best effort by design: the profile may name files from a tree that is not
// checked out here — a CI artifact merged locally, say — and a missing file
// must degrade to a bare location rather than fail the report.
func annotate(gaps []Gap, root, module string) {
	cache := map[string][]string{}
	for i := range gaps {
		lines, ok := cache[gaps[i].File]
		if !ok {
			lines = readLines(DiskPath(root, module, gaps[i].File))
			cache[gaps[i].File] = lines
		}
		if n := gaps[i].Line; n >= 1 && n <= len(lines) {
			gaps[i].Source = strings.TrimSpace(lines[n-1])
		}
	}
}

func readLines(pth string) []string {
	f, err := os.Open(pth)
	if err != nil {
		return nil
	}
	defer f.Close()

	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if sc.Err() != nil {
		// A half-read file would annotate gaps with the wrong source lines,
		// which is worse than annotating none of them.
		return nil
	}
	return lines
}

// DiskPath turns a profile's import-path-qualified name into a file on disk.
//
// Profiles name files by import path, which is what makes them portable across
// machines; anything that has to read the source — an annotated gap list, an
// lcov file genhtml can render — has to undo that against the module root.
func DiskPath(root, module, file string) string {
	rel := file
	if module != "" {
		if trimmed, ok := strings.CutPrefix(file, module+"/"); ok {
			rel = trimmed
		}
	}
	return filepath.Join(root, filepath.FromSlash(rel))
}

func WriteGaps(w io.Writer, gaps []Gap, limit int) {
	bw := bufio.NewWriter(w)
	defer bw.Flush()

	if len(gaps) == 0 {
		fmt.Fprintln(bw, "no uncovered statements")
		return
	}
	shown := gaps
	if limit > 0 && limit < len(shown) {
		shown = shown[:limit]
	}

	total := 0
	for _, g := range gaps {
		total += g.NumStmt
	}
	for _, g := range shown {
		fmt.Fprintf(bw, "%4d  %s:%d:%d", g.NumStmt, g.File, g.Line, g.Col)
		if g.Source != "" {
			fmt.Fprintf(bw, "\n      %s", g.Source)
		}
		fmt.Fprintln(bw)
	}
	fmt.Fprintf(bw, "\n%d uncovered statements in %d blocks", total, len(gaps))
	if len(shown) < len(gaps) {
		fmt.Fprintf(bw, " (showing %d)", len(shown))
	}
	fmt.Fprintln(bw)
}

// WriteLCOV emits lcov tracefile format.
//
// The point is to reach the OSS coverage viewers that already exist — genhtml,
// and anything else that reads lcov — without this repo growing a dashboard of
// its own.
//
// The conversion is lossy in one direction worth knowing about: Go measures
// statement *blocks* spanning line ranges, lcov measures lines. Every line of a
// block inherits the block's count, so a line holding two blocks reports the
// busier one. That reads correctly for "which lines never ran", which is all
// the viewers are being asked.
func WriteLCOV(w io.Writer, p *Profile, ex *Excluder, root, module string) error {
	bw := bufio.NewWriter(w)
	defer bw.Flush()

	byFile := map[string]map[int]int{}
	var order []string
	for _, b := range p.Blocks() {
		if ex.Excluded(b.File) {
			continue
		}
		lines, ok := byFile[b.File]
		if !ok {
			lines = map[int]int{}
			byFile[b.File] = lines
			order = append(order, b.File)
		}
		for ln := b.StartLine; ln <= b.EndLine; ln++ {
			if b.Count > lines[ln] {
				lines[ln] = b.Count
			} else if _, seen := lines[ln]; !seen {
				lines[ln] = b.Count
			}
		}
	}
	sort.Strings(order)

	fmt.Fprintln(bw, "TN:")
	for _, file := range order {
		lines := byFile[file]
		nums := make([]int, 0, len(lines))
		for ln := range lines {
			nums = append(nums, ln)
		}
		sort.Ints(nums)

		fmt.Fprintf(bw, "SF:%s\n", DiskPath(root, module, file))
		hit := 0
		for _, ln := range nums {
			fmt.Fprintf(bw, "DA:%d,%d\n", ln, lines[ln])
			if lines[ln] > 0 {
				hit++
			}
		}
		fmt.Fprintf(bw, "LF:%d\n", len(nums))
		fmt.Fprintf(bw, "LH:%d\n", hit)
		fmt.Fprintln(bw, "end_of_record")
	}
	return bw.Flush()
}
