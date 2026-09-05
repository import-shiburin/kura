package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path"
	"sort"
	"strings"
)

// Config is the coverage contract, kept in .covcheck.json next to go.mod.
//
// Per-package floors rather than one global number, because a single total is
// the wrong instrument for a 100% target: it lets a well-tested package pay for
// a neglected one, and it gives no signal about *where* to work. The floors
// ratchet (see the ratchet subcommand), so coverage can only be traded upward.
type Config struct {
	// Comment carries the file's own explanation. JSON has nowhere else to put
	// one, and a contract nobody can annotate is a contract nobody argues with.
	Comment []string `json:"comment,omitempty"`

	// Goal is where the floors are headed. Nothing enforces it — it is the
	// target the ratchet is walking towards, recorded so the distance left is
	// visible in the same file as the distance covered.
	Goal float64 `json:"goal,omitempty"`

	// Total is the floor for the whole module.
	Total float64 `json:"total"`

	// Default applies to any package with no explicit entry, so a new package
	// cannot arrive under the radar at 0%.
	Default float64 `json:"default"`

	// Packages maps import path to its own floor.
	Packages map[string]float64 `json:"packages,omitempty"`

	// Exclude drops files from both numerator and denominator.
	//
	// Every entry carries a reason. An exclusion without one is how a coverage
	// target rots: six months later nobody remembers whether the file was
	// untestable or merely inconvenient, and the safe move is always to leave
	// it excluded.
	Exclude []Exclusion `json:"exclude,omitempty"`
}

// Exclusion removes matching files from measurement.
type Exclusion struct {
	// Pattern matches the profile's import-path-qualified file name using
	// path.Match semantics, extended so that a trailing "/..." matches a whole
	// subtree the way Go's own tool patterns do.
	Pattern string `json:"pattern"`
	Reason  string `json:"reason"`
}

// DefaultConfig is what `covcheck init` writes when there is no config yet.
func DefaultConfig() *Config {
	return &Config{Total: 0, Default: 0, Packages: map[string]float64{}}
}

func LoadConfig(pth string) (*Config, error) {
	data, err := os.ReadFile(pth)
	if err != nil {
		return nil, err
	}
	cfg := DefaultConfig()
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", pth, err)
	}
	if cfg.Packages == nil {
		cfg.Packages = map[string]float64{}
	}
	for i, e := range cfg.Exclude {
		if e.Reason == "" {
			return nil, fmt.Errorf("%s: exclude[%d] %q has no reason", pth, i, e.Pattern)
		}
	}
	return cfg, nil
}

// Save writes the config back, formatted so a diff of a ratchet is readable.
func (c *Config) Save(pth string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(pth, append(data, '\n'), 0o644)
}

// Threshold is the floor that applies to one package.
func (c *Config) Threshold(pkg string) float64 {
	if v, ok := c.Packages[pkg]; ok {
		return v
	}
	return c.Default
}

// Excluder answers whether a file is measured, with the patterns compiled once.
type Excluder struct {
	exact    map[string]bool
	prefixes []string
	globs    []string
}

func (c *Config) Excluder() (*Excluder, error) {
	ex := &Excluder{exact: map[string]bool{}}
	for _, e := range c.Exclude {
		p := strings.TrimSpace(e.Pattern)
		switch {
		case p == "":
			return nil, fmt.Errorf("empty exclude pattern")
		case strings.HasSuffix(p, "/..."):
			ex.prefixes = append(ex.prefixes, strings.TrimSuffix(p, "..."))
		case strings.ContainsAny(p, "*?["):
			// Fail early on a malformed glob rather than silently matching
			// nothing for the life of the config.
			if _, err := path.Match(p, "probe"); err != nil {
				return nil, fmt.Errorf("exclude pattern %q: %w", p, err)
			}
			ex.globs = append(ex.globs, p)
		default:
			ex.exact[p] = true
		}
	}
	return ex, nil
}

func (e *Excluder) Excluded(file string) bool {
	if e == nil {
		return false
	}
	if e.exact[file] {
		return true
	}
	for _, p := range e.prefixes {
		if strings.HasPrefix(file, p) {
			return true
		}
	}
	for _, g := range e.globs {
		if ok, _ := path.Match(g, file); ok {
			return true
		}
		// A glob without a slash is meant to match the base name, which is how
		// people write "*_gen.go" and expect it to work anywhere in the tree.
		if !strings.Contains(g, "/") {
			if ok, _ := path.Match(g, path.Base(file)); ok {
				return true
			}
		}
	}
	return false
}

// Ratchet raises every floor to what the summary actually achieved.
//
// Floors only go up. A drop is what the gate is for; if coverage genuinely has
// to fall — a package deleted, a feature cut — the floor is edited by hand and
// the diff carries the argument. Automating the fall would quietly undo the
// whole point.
func (c *Config) Ratchet(s *Summary, round float64) (changed []string) {
	if v := floor(s.Total.Percent(), round); v > c.Total {
		changed = append(changed, fmt.Sprintf("total %.2f%% -> %.2f%%", c.Total, v))
		c.Total = v
	}
	for _, pkg := range s.PackageNames() {
		got := floor(s.Packages[pkg].Percent(), round)
		if got > c.Threshold(pkg) {
			changed = append(changed, fmt.Sprintf("%s %.2f%% -> %.2f%%", pkg, c.Threshold(pkg), got))
			c.Packages[pkg] = got
		}
	}
	sort.Strings(changed)
	return changed
}

// floor rounds down to a multiple of round, so a floor is never set above what
// was measured. Rounding up by even a hundredth of a point makes the next run
// fail on nothing but float noise.
//
// Dividing rather than multiplying at the end matters more than it looks:
// float64(int(v/round)) * round writes 84.10000000000001 into a file people
// read and hand-edit, because 0.1 is not representable and the multiply rounds
// away from the value. Scaling by 1/round and dividing lands on the shortest
// decimal that round-trips, which is what JSON then prints.
func floor(v, round float64) float64 {
	if round <= 0 {
		return v
	}
	scale := 1 / round
	// The epsilon only rescues a value that is a hair under an exact multiple
	// because of the scaling itself; it is far below the gate's tolerance, so
	// it cannot lift a floor above what was measured.
	return math.Floor(v*scale+1e-9) / scale
}
