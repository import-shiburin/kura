package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// mountInfoPath is the kernel's account of what is mounted where. A variable so
// a test can hand the parser a fixture: the interesting cases — a stacked
// mount, a path with a space in it — cannot be arranged on the host running the
// tests.
var mountInfoPath = "/proc/self/mountinfo"

// hiddenAnyDir lets a caller point -mount at an ordinary directory.
//
// It is deliberately never registered with the flag package, which is what
// keeps it out of every -h. The check it defeats is the one that catches an
// unmounted cartridge, so an operator who finds this in a help listing is being
// invited to lose a volume; the test suite and a developer working against a
// temp directory are the only callers it is for.
const hiddenAnyDir = "allow-any-dir"

// mount is one line of mountinfo, reduced to what we ask of it.
type mount struct {
	point  string
	fstype string
	source string
}

// takeHiddenFlag strips an unregistered boolean flag out of argv and reports
// its value. Parsing it here rather than declaring it means flag.PrintDefaults
// cannot list it and `kura write -h` cannot mention it.
func takeHiddenFlag(argv []string, name string) ([]string, bool) {
	kept := make([]string, 0, len(argv))
	set := false
	for _, arg := range argv {
		body, ok := strings.CutPrefix(arg, "--")
		if !ok {
			body, ok = strings.CutPrefix(arg, "-")
		}
		if !ok {
			kept = append(kept, arg)
			continue
		}
		switch key, value, hasValue := strings.Cut(body, "="); {
		case key != name:
			kept = append(kept, arg)
		case !hasValue:
			set = true
		default:
			// An explicit -allow-any-dir=false is still consumed. Letting it
			// through to Parse would report an undefined flag, which is a
			// confusing answer to a flag the caller clearly knows about.
			b, err := strconv.ParseBool(value)
			set = err == nil && b
		}
	}
	return kept, set
}

// requireMountPoint refuses a -mount that is not the root of a mounted
// filesystem.
//
// The failure this exists for is a typo, or a cartridge nobody mounted. Either
// leaves an ordinary directory on the host's own disk, and because a directory
// is exactly what the file backend expects to be handed, everything downstream
// agrees: objects are written, the catalog records a volume as safely stored,
// and the write exits zero. Nothing is on any medium, and the archive does not
// find out until a restore. No later check can catch it either — by then the
// only evidence, that the path was not a mount, is gone.
func requireMountPoint(path string, allowAny bool) error {
	if allowAny {
		return nil
	}
	// The kernel records mount points resolved, so a path reached through a
	// symlink has to be resolved too or it matches nothing.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("-mount %s: %w", path, err)
	}
	mounts, err := readMounts()
	if err != nil {
		// The check itself is broken rather than failing, so this is the one
		// place the escape hatch is named: an operator on a system without
		// mountinfo would otherwise be stopped with no documented way past a
		// check that cannot run. A refusal from checkMountPoint stays silent
		// about it, because there the check is working and being right.
		return fmt.Errorf("cannot tell whether %s is a mount point: %w\n"+
			"       pass -allow-any-dir to write to it anyway, having checked it yourself", path, err)
	}
	return checkMountPoint(path, resolved, mounts)
}

// checkMountPoint is the decision, kept separate from reading the host so the
// cases worth testing — a mount point with a space in it, a path sitting two
// mounts deep — can be put to it as a fixture rather than depending on whatever
// the machine running the tests happens to have mounted.
func checkMountPoint(named, resolved string, mounts []mount) error {
	var holder mount
	for _, m := range mounts {
		if m.point == resolved {
			return nil
		}
		// Longest match wins: /mnt/lto sits under both / and /mnt when both are
		// mounts, and only the nearer one says where the bytes would land.
		if under(resolved, m.point) && len(m.point) > len(holder.point) {
			holder = m
		}
	}

	where := "the host's filesystem"
	if holder.point != "" {
		where = fmt.Sprintf("%s, which is %s on %s", holder.point, holder.fstype, holder.source)
	}
	return fmt.Errorf("-mount %s is not a mount point: it is a directory on %s.\n"+
		"       Load the cartridge and mount it there first — writing to an unmounted\n"+
		"       path fills the host's disk and records a volume that is on no medium",
		named, where)
}

// under reports whether path lies inside dir.
func under(path, dir string) bool {
	if dir == "/" {
		return strings.HasPrefix(path, "/")
	}
	return strings.HasPrefix(path, dir+"/")
}

// readMounts parses mountinfo. The format is positional up to the mount point
// and then variable: a run of optional fields sits between the per-mount
// options and a lone "-", which is why the separator exists at all and why the
// filesystem type cannot be addressed by a fixed index.
func readMounts() ([]mount, error) {
	f, err := os.Open(mountInfoPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var mounts []mount
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 7 {
			continue
		}
		m := mount{point: unescapeMountField(fields[4])}
		for i := 6; i < len(fields)-2; i++ {
			if fields[i] == "-" {
				m.fstype, m.source = fields[i+1], unescapeMountField(fields[i+2])
				break
			}
		}
		mounts = append(mounts, m)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return mounts, nil
}

// unescapeMountField undoes the octal escaping the kernel applies to paths in
// mountinfo: space, tab, newline and backslash are written \040 \011 \012 \134.
// Without this a mount point containing a space arrives as two fields and is
// compared against nothing, so a real mount would be refused as if it were a
// bare directory.
func unescapeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 4
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
