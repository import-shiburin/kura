package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A fixture rather than the host's own mounts, because the cases that matter
// cannot be arranged on a test machine without root. Line by line: a mount with
// optional fields, one without any, the root, and a mount point holding a
// space. The missing optional field on /mnt is the trap — it moves the "-"
// separator, and with it the filesystem type, one column left.
const mountInfoFixture = `21 26 0:20 / /sys rw,nosuid,nodev,noexec,relatime shared:7 - sysfs sysfs rw
26 1 8:2 / / rw,relatime shared:1 - ext4 /dev/sda2 rw
40 26 8:17 / /mnt rw,relatime - ext4 /dev/sdb1 rw
41 40 8:33 / /mnt/lto\040spare rw,relatime shared:21 - ext4 /dev/sdc1 rw
`

func fixtureMounts(t *testing.T) []mount {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(path, []byte(mountInfoFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	old := mountInfoPath
	mountInfoPath = path
	t.Cleanup(func() { mountInfoPath = old })

	mounts, err := readMounts()
	if err != nil {
		t.Fatal(err)
	}
	return mounts
}

// The whole point of the check: a path nobody mounted is refused before a
// single object is written. Getting this wrong is silent — the file backend is
// handed a directory either way, the write succeeds, and the catalog records a
// volume that exists on no medium.
func TestAnUnmountedPathIsRefused(t *testing.T) {
	err := checkMountPoint("/mnt/lto", "/mnt/lto", fixtureMounts(t))
	if err == nil {
		t.Fatal("a directory that is not a mount point was accepted")
	}
	if !strings.Contains(err.Error(), "not a mount point") {
		t.Errorf("error does not say what is wrong: %v", err)
	}
}

// The error has to name the filesystem the bytes would have gone to, not just
// refuse. An operator who mistyped /mnt/lto needs to know the write was about
// to land on /dev/sdb1, and /mnt is the answer rather than / — the nearest
// enclosing mount is the one that would have filled up.
func TestTheRefusalNamesTheFilesystemThatWouldHaveTakenTheBytes(t *testing.T) {
	err := checkMountPoint("/mnt/lto", "/mnt/lto", fixtureMounts(t))
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, want := range []string{"/mnt", "ext4", "/dev/sdb1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q, got: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "/dev/sda2") {
		t.Errorf("named the root filesystem instead of the nearest mount: %v", err)
	}
}

// A real mount point is accepted, including one whose path the kernel escapes.
// Without the unescaping this reads as two fields, matches nothing, and a
// correctly mounted cartridge is refused — the check turning on its user.
func TestAMountPointIsAcceptedEvenWithASpaceInItsPath(t *testing.T) {
	mounts := fixtureMounts(t)
	for _, point := range []string{"/", "/mnt", "/mnt/lto spare"} {
		if err := checkMountPoint(point, point, mounts); err != nil {
			t.Errorf("%q is a mount point in the fixture but was refused: %v", point, err)
		}
	}
}

// The separator between the optional fields and the filesystem type is not at a
// fixed column: /sys carries a shared: tag and /mnt carries none. Reading the
// type by index instead of by searching for the "-" gets one of them wrong.
func TestFilesystemTypeSurvivesTheVariableOptionalFields(t *testing.T) {
	want := map[string]struct{ fstype, source string }{
		"/sys":           {"sysfs", "sysfs"},
		"/":              {"ext4", "/dev/sda2"},
		"/mnt":           {"ext4", "/dev/sdb1"},
		"/mnt/lto spare": {"ext4", "/dev/sdc1"},
	}
	for _, m := range fixtureMounts(t) {
		w, ok := want[m.point]
		if !ok {
			t.Errorf("unexpected mount point parsed: %q", m.point)
			continue
		}
		if m.fstype != w.fstype || m.source != w.source {
			t.Errorf("%s: got %s on %s, want %s on %s", m.point, m.fstype, m.source, w.fstype, w.source)
		}
		delete(want, m.point)
	}
	for point := range want {
		t.Errorf("mount point missing from the parse: %q", point)
	}
}

// The escape hatch, which is the only reason a developer can point a command at
// a temp directory. It has to work without consulting mountinfo at all, since
// the whole point is that the path is not in there.
func TestTheHiddenFlagAllowsAnOrdinaryDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := requireMountPoint(dir, false); err == nil {
		t.Fatal("a temp directory was accepted as a mount point")
	}
	if err := requireMountPoint(dir, true); err != nil {
		t.Errorf("the hidden flag did not allow an ordinary directory: %v", err)
	}
}

// Against the host rather than a fixture: / is a mount point everywhere this
// runs, so the reader and the check agree on at least one real answer.
func TestTheRootFilesystemIsAMountPointOnTheHost(t *testing.T) {
	if err := requireMountPoint("/", false); err != nil {
		t.Errorf("/ should be a mount point: %v", err)
	}
}

// A path that does not exist is refused for that reason rather than being
// reported as "not a mount point", which would send an operator looking for a
// mount when the real answer is a typo.
func TestAMissingPathIsRefusedAsMissing(t *testing.T) {
	err := requireMountPoint(filepath.Join(t.TempDir(), "nope"), false)
	if err == nil {
		t.Fatal("a nonexistent path was accepted")
	}
	if strings.Contains(err.Error(), "not a mount point") {
		t.Errorf("a missing path should not be reported as an unmounted one: %v", err)
	}
}

// The flag is parsed out of argv rather than registered, which is what keeps it
// out of every -h. That only holds if the stripping is exact: it must consume
// its own spellings, leave every other argument untouched and in order, and not
// swallow a value that merely starts the same way.
func TestTheHiddenFlagIsStrippedFromArgvAndNothingElseIs(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want []string
		set  bool
	}{
		{"absent", []string{"-mount", "/mnt/lto"}, []string{"-mount", "/mnt/lto"}, false},
		{"single dash", []string{"-allow-any-dir", "-quiet"}, []string{"-quiet"}, true},
		{"double dash", []string{"--allow-any-dir"}, []string{}, true},
		{"explicit true", []string{"-allow-any-dir=true"}, []string{}, true},
		{"explicit false is still consumed", []string{"-allow-any-dir=false"}, []string{}, false},
		{"a flag with the same prefix survives", []string{"-allow-any-directory"}, []string{"-allow-any-directory"}, false},
		{"a value that looks like the flag survives", []string{"-mount", "allow-any-dir"}, []string{"-mount", "allow-any-dir"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, set := takeHiddenFlag(tc.argv, hiddenAnyDir)
			if set != tc.set {
				t.Errorf("set = %v, want %v", set, tc.set)
			}
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("argv = %v, want %v", got, tc.want)
			}
		})
	}
}
