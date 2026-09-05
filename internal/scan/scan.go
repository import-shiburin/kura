// Package scan walks the source trees and works out what has changed since the
// last generation.
//
// It is the read-only half of a cycle: nothing here touches media or the
// catalog on disk. The output is a Delta — what is new, what changed, what can
// be recorded against bytes already written, and what has gone away — which is
// all the planner needs to decide what a cycle has to write.
package scan

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
)

// Root is a directory to walk, optionally recorded under a different path.
//
// The two differ when reading from a filesystem snapshot. A ZFS snapshot lives
// at /storage/.zfs/snapshot/<label>, and the label changes every cycle: scanned
// as-is, every file would look new and the whole incremental scheme collapses.
// Recording them under the live path keeps identity stable across cycles.
type Root struct {
	Scan string // where to read from
	As   string // what to call it in the catalog; empty means the same as Scan
}

// Canonical is the path prefix recorded in the catalog.
func (r Root) Canonical() string {
	if r.As != "" {
		return r.As
	}
	return r.Scan
}

// rewrite maps a path under r.Scan to its canonical form.
func (r Root) rewrite(path string) string {
	if r.As == "" {
		return path
	}
	return r.As + strings.TrimPrefix(path, r.Scan)
}

// ParseRoot accepts "path" or "path=canonical".
func ParseRoot(s string) (Root, error) {
	scan, as, found := strings.Cut(s, "=")
	scan = strings.TrimSpace(scan)
	if scan == "" {
		return Root{}, fmt.Errorf("empty root")
	}
	r := Root{Scan: filepath.Clean(scan)}
	if found {
		as = strings.TrimSpace(as)
		if as == "" {
			return Root{}, fmt.Errorf("%q: empty canonical path after =", s)
		}
		r.As = filepath.Clean(as)
	}
	return r, nil
}

// CanonicalRoots returns the catalog-facing prefix of each root.
func CanonicalRoots(roots []Root) []string {
	out := make([]string, len(roots))
	for i, r := range roots {
		out[i] = r.Canonical()
	}
	return out
}

// File is one entry found on disk during the walk.
type File struct {
	// Path is the canonical path: the file's identity in the catalog.
	Path string
	// Real is where the bytes actually are right now, set only when it differs
	// from Path. Anything opening the file must go through OpenPath.
	Real string

	Size    int64
	ModTime int64

	// CTime is the inode change time, in nanoseconds. Unlike mtime it cannot be
	// set by userspace, which makes it the only reliable signal that a file
	// changed underneath a program that restored its mtime.
	//
	// Nanoseconds rather than seconds because that is exactly the case it has
	// to catch: a rewrite in the same second as the previous scan is invisible
	// at second granularity, which is the situation mtime already fails at.
	CTime int64

	Mode  fs.FileMode
	Link  string // symlink target
	Dev   uint64
	Ino   uint64
	Nlink uint64

	// HardLinkTo is set when an earlier entry in this scan already covers the
	// same inode, so only one copy needs to reach media.
	HardLinkTo string

	// Hash is the sampled digest, filled in only when this run computed one.
	Hash string
}

// IsRegular reports whether the entry carries data that must be written.
func (f *File) IsRegular() bool { return f.Mode.IsRegular() && f.HardLinkTo == "" }

// OpenPath is where the file must be read from, which is not the same as its
// catalog identity when scanning a snapshot.
func (f *File) OpenPath() string {
	if f.Real != "" {
		return f.Real
	}
	return f.Path
}

// Excluder decides which paths the walk skips.
//
// Patterns follow the same shapes the old tar scripts used:
//   - a rooted path with no glob metacharacters excludes that path and its subtree
//     (/storage/Archive)
//   - a rooted path with metacharacters is matched against the full path
//     (/storage/*/cache)
//   - anything else is matched against the basename (*.tmp, lost+found)
type Excluder struct {
	prefixes []string
	globs    []string
	names    []string
}

var vcsNames = []string{".git", ".svn", ".hg", ".bzr", "CVS", ".gitignore", ".gitattributes"}

// NewExcluder builds an Excluder from raw pattern strings.
func NewExcluder(patterns []string, excludeVCS bool) *Excluder {
	e := new(Excluder)
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		switch {
		case strings.HasPrefix(p, "/") && !hasMeta(p):
			e.prefixes = append(e.prefixes, filepath.Clean(p))
		case strings.HasPrefix(p, "/"):
			e.globs = append(e.globs, filepath.Clean(p))
		default:
			e.names = append(e.names, p)
		}
	}
	if excludeVCS {
		e.names = append(e.names, vcsNames...)
	}
	return e
}

func hasMeta(s string) bool { return strings.ContainsAny(s, "*?[") }

// Match reports whether path is excluded.
func (e *Excluder) Match(path string) bool {
	for _, p := range e.prefixes {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	for _, g := range e.globs {
		if ok, _ := filepath.Match(g, path); ok {
			return true
		}
	}
	base := filepath.Base(path)
	for _, n := range e.names {
		if ok, _ := filepath.Match(n, base); ok {
			return true
		}
	}
	return false
}

// LoadPatterns reads one pattern per line, ignoring blanks and # comments.
func LoadPatterns(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out, sc.Err()
}

// Stats records what the walk saw, including what it had to skip.
type Stats struct {
	Files     int64
	Bytes     int64
	Dirs      int64
	Symlinks  int64
	HardLinks int64
	Excluded  int64
	Errors    int64

	// SkippedRoots lists roots dropped as redundant, so the report can say so
	// rather than silently scanning less than was asked for.
	SkippedRoots []string
}

// dedupeRoots drops any root already covered by another.
//
// Passing both /storage and /storage/Music would otherwise walk the Music
// subtree twice, and every file in it would be counted twice, packed onto two
// volumes, and land in the catalog as a duplicate path. Symlinks are resolved
// for the comparison only, so two names for one directory collapse too, while
// the path actually recorded stays the one that was asked for.
func dedupeRoots(roots []Root) (kept []Root, dropped []string) {
	type entry struct {
		root     Root
		resolved string
	}

	seen := make(map[string]bool)
	var cleaned []entry
	for _, r := range roots {
		r.Scan = filepath.Clean(r.Scan)
		resolved, err := filepath.EvalSymlinks(r.Scan)
		if err != nil {
			resolved = r.Scan // unreadable roots surface during the walk
		}
		if seen[resolved] {
			dropped = append(dropped, r.Scan)
			continue
		}
		seen[resolved] = true
		cleaned = append(cleaned, entry{r, resolved})
	}

	// Sorting by resolved path puts a parent immediately before its children.
	sort.Slice(cleaned, func(i, j int) bool { return cleaned[i].resolved < cleaned[j].resolved })

	var keptResolved []string
	for _, e := range cleaned {
		covered := false
		for _, k := range keptResolved {
			if e.resolved == k || strings.HasPrefix(e.resolved, k+"/") {
				covered = true
				break
			}
		}
		if covered {
			dropped = append(dropped, e.root.Scan)
			continue
		}
		keptResolved = append(keptResolved, e.resolved)
		kept = append(kept, e.root)
	}
	return kept, dropped
}

// Walk visits every root and returns the entries that survive exclusion.
//
// Symlinks are recorded but never followed, so a link pointing back into the
// tree cannot cause the walk to loop or double-count.
func Walk(roots []Root, ex *Excluder, onErr func(string, error)) ([]*File, *Stats, error) {
	stats := new(Stats)
	var out []*File
	seen := make(map[[2]uint64]string) // (dev, ino) -> first path holding the data

	roots, stats.SkippedRoots = dedupeRoots(roots)

	for _, root := range roots {
		err := filepath.WalkDir(root.Scan, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				atomic.AddInt64(&stats.Errors, 1)
				onErr(path, err)
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			// Exclusions are matched against the canonical path, so an exclude
			// file written for /storage keeps working when the scan actually
			// runs against a snapshot of it.
			canonical := root.rewrite(path)
			if path != root.Scan && ex.Match(canonical) {
				stats.Excluded++
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				stats.Dirs++
				return nil
			}

			info, err := d.Info()
			if err != nil {
				stats.Errors++
				onErr(path, err)
				return nil
			}

			sf := &File{
				Path:    canonical,
				Size:    info.Size(),
				ModTime: info.ModTime().Unix(),
				Mode:    info.Mode(),
			}
			if canonical != path {
				sf.Real = path
			}

			if info.Mode()&fs.ModeSymlink != 0 {
				if target, err := os.Readlink(path); err == nil {
					sf.Link = target
				}

				sf.Size = 0
				stats.Symlinks++
				out = append(out, sf)
				return nil
			}

			if !info.Mode().IsRegular() {
				// Sockets, devices, fifos: recorded so restore can recreate
				// them, but they carry no data.
				sf.Size = 0
				out = append(out, sf)
				return nil
			}

			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				sf.Dev, sf.Ino, sf.Nlink = uint64(st.Dev), st.Ino, uint64(st.Nlink)
				sf.CTime = st.Ctim.Nano()
				if sf.Nlink > 1 {
					key := [2]uint64{sf.Dev, sf.Ino}
					if first, dup := seen[key]; dup {
						sf.HardLinkTo = first // canonical path of the copy holding the data
						stats.HardLinks++
						out = append(out, sf)
						return nil
					}
					seen[key] = canonical
				}
			}

			stats.Files++
			stats.Bytes += sf.Size
			out = append(out, sf)
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
	}
	return out, stats, nil
}
