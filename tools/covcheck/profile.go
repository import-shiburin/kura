package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Block is one coverage-profile record: a half-open span of source with a
// statement count and how many times it ran.
//
// The profile's file field is import-path qualified ("kura/internal/plan/pack.go"),
// not a filesystem path. Keeping it verbatim matters: it is the only thing that
// makes blocks from different pods comparable, and turning it into a disk path
// is a separate step that needs the module root.
type Block struct {
	File      string
	StartLine int
	StartCol  int
	EndLine   int
	EndCol    int
	NumStmt   int
	Count     int
}

// Covered reports whether anything ever executed this block.
//
// Deliberately a threshold rather than a count. Go's "set" and "count" counter
// modes disagree about what Count means — set writes 1, count writes the real
// tally — and every question covcheck answers is about reachability, not hot
// paths, so both collapse to the same boolean.
func (b Block) Covered() bool { return b.Count > 0 }

// Key identifies a block independently of which pod reported it, so the same
// span seen by a unit test and by the e2e binary merges instead of double
// counting.
func (b Block) Key() string {
	return fmt.Sprintf("%s:%d.%d,%d.%d", b.File, b.StartLine, b.StartCol, b.EndLine, b.EndCol)
}

// Package is the import path of the directory holding the block's file.
func (b Block) Package() string { return path.Dir(b.File) }

// Profile is a merged set of coverage blocks.
//
// `go tool covdata textfmt` already unions the raw pods, but profiles also
// arrive as plain text files — a stashed baseline, a CI artifact, the output of
// an older run — and those have to be mergeable too. So the merge lives here
// rather than being assumed upstream.
type Profile struct {
	Mode   string
	blocks map[string]Block
}

func NewProfile() *Profile { return &Profile{blocks: map[string]Block{}} }

// ParseProfile reads one text-format coverage profile.
func ParseProfile(r io.Reader) (*Profile, error) {
	p := NewProfile()
	sc := bufio.NewScanner(r)
	// Generated Go can carry very long lines; the default 64 KiB token limit
	// is not enough for the paths a deep source tree produces.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(text, "mode:"); ok {
			p.Mode = strings.TrimSpace(rest)
			continue
		}
		b, err := parseBlock(text)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		p.Add(b)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return p, nil
}

// Add merges one block, keeping the highest count seen for that span.
//
// Max, not sum: a block covered by a unit test and again by an e2e run is
// covered once. Summing would still answer "is it covered" correctly but would
// make the count field lie, and `uncovered` sorts on neighbouring data.
func (p *Profile) Add(b Block) {
	k := b.Key()
	if prev, ok := p.blocks[k]; ok && prev.Count > b.Count {
		return
	}
	p.blocks[k] = b
}

// Merge folds another profile into this one.
func (p *Profile) Merge(other *Profile) {
	if p.Mode == "" {
		p.Mode = other.Mode
	}
	for _, b := range other.blocks {
		p.Add(b)
	}
}

// Blocks returns every block in a stable order: by file, then by position.
func (p *Profile) Blocks() []Block {
	out := make([]Block, 0, len(p.blocks))
	for _, b := range p.blocks {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		if out[i].StartLine != out[j].StartLine {
			return out[i].StartLine < out[j].StartLine
		}
		return out[i].StartCol < out[j].StartCol
	})
	return out
}

// Write emits the profile in the text format `go tool cover` accepts.
func (p *Profile) Write(w io.Writer) error {
	mode := p.Mode
	if mode == "" {
		mode = "set"
	}
	bw := bufio.NewWriter(w)
	fmt.Fprintf(bw, "mode: %s\n", mode)
	for _, b := range p.Blocks() {
		fmt.Fprintf(bw, "%s:%d.%d,%d.%d %d %d\n",
			b.File, b.StartLine, b.StartCol, b.EndLine, b.EndCol, b.NumStmt, b.Count)
	}
	return bw.Flush()
}

// parseBlock reads one profile record.
//
// The format is "path:sl.sc,el.ec numstmt count", and the path may contain
// colons on some platforms, so the span is found from the right.
func parseBlock(s string) (Block, error) {
	var b Block

	fields := strings.Fields(s)
	if len(fields) < 3 {
		return b, fmt.Errorf("want 3 fields, got %d in %q", len(fields), s)
	}
	countStr := fields[len(fields)-1]
	stmtStr := fields[len(fields)-2]
	loc := strings.Join(fields[:len(fields)-2], " ")

	colon := strings.LastIndex(loc, ":")
	if colon < 0 {
		return b, fmt.Errorf("no position in %q", loc)
	}
	b.File = loc[:colon]

	span := loc[colon+1:]
	start, end, ok := strings.Cut(span, ",")
	if !ok {
		return b, fmt.Errorf("no span separator in %q", span)
	}

	var err error
	if b.StartLine, b.StartCol, err = parsePos(start); err != nil {
		return b, err
	}
	if b.EndLine, b.EndCol, err = parsePos(end); err != nil {
		return b, err
	}
	if b.NumStmt, err = strconv.Atoi(stmtStr); err != nil {
		return b, fmt.Errorf("statement count %q: %w", stmtStr, err)
	}
	if b.Count, err = strconv.Atoi(countStr); err != nil {
		return b, fmt.Errorf("execution count %q: %w", countStr, err)
	}
	return b, nil
}

func parsePos(s string) (line, col int, err error) {
	l, c, ok := strings.Cut(s, ".")
	if !ok {
		return 0, 0, fmt.Errorf("no line.col in %q", s)
	}
	if line, err = strconv.Atoi(l); err != nil {
		return 0, 0, fmt.Errorf("line %q: %w", l, err)
	}
	if col, err = strconv.Atoi(c); err != nil {
		return 0, 0, fmt.Errorf("column %q: %w", c, err)
	}
	return line, col, nil
}

// LoadProfiles reads and merges every named profile.
func LoadProfiles(paths []string) (*Profile, error) {
	merged := NewProfile()
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		prof, err := ParseProfile(f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		merged.Merge(prof)
	}
	return merged, nil
}

// Tally is a covered-out-of-total count.
type Tally struct {
	Covered int
	Total   int
}

func (t Tally) Percent() float64 {
	if t.Total == 0 {
		// An empty package is vacuously complete. Reporting 0% would make a
		// package of nothing but type declarations fail a 100% gate forever.
		return 100
	}
	return float64(t.Covered) / float64(t.Total) * 100
}

func (t *Tally) add(b Block) {
	t.Total += b.NumStmt
	if b.Covered() {
		t.Covered += b.NumStmt
	}
}

// Summary is the profile reduced to per-package and per-file tallies.
type Summary struct {
	Total    Tally
	Packages map[string]Tally
	Files    map[string]Tally
}

// Summarise tallies the profile, skipping anything the config excludes.
func (p *Profile) Summarise(ex *Excluder) *Summary {
	s := &Summary{
		Packages: map[string]Tally{},
		Files:    map[string]Tally{},
	}
	for _, b := range p.Blocks() {
		if ex.Excluded(b.File) {
			continue
		}
		s.Total.add(b)

		pkg := s.Packages[b.Package()]
		pkg.add(b)
		s.Packages[b.Package()] = pkg

		file := s.Files[b.File]
		file.add(b)
		s.Files[b.File] = file
	}
	return s
}

// PackageNames lists the summarised packages in import-path order.
func (s *Summary) PackageNames() []string {
	out := make([]string, 0, len(s.Packages))
	for k := range s.Packages {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// FileNames lists the summarised files in import-path order.
func (s *Summary) FileNames() []string {
	out := make([]string, 0, len(s.Files))
	for k := range s.Files {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
