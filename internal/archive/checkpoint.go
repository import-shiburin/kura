package archive

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"kura/internal/catalog"
)

// ResumeFormat tags the checkpoint a paused write leaves behind. A build that
// does not recognise it ignores the file and resumes the slow way, which is the
// safe direction to fail in.
const ResumeFormat = "kura-resume-1"

// Checkpoint is what a deliberately paused write leaves behind, and the only
// thing that tells a pause from a crash.
//
// It holds the records the run computed but has not committed: a pause cannot
// write the catalog, locally or onto the medium, without either misdirecting the
// next run's choice of volume or leaving the cartridge indexing half a cycle
// (DESIGN §11.1). So the records wait here instead.
//
// It is written only after volume.Sync has returned, which is what makes its
// presence a statement about durability rather than about intent. A host that
// powers off between the objects and the Sync leaves no checkpoint, and its
// resume distrusts the medium exactly as a crash's does.
type Checkpoint struct {
	// Objects is every object this run placed, in the order it placed them.
	//
	// It carries `json:"-"` because it is not part of the header line: encoding
	// the Checkpoint itself produces exactly the header, and each Placed is then
	// a line of its own. One type, two shapes, no second struct whose field list
	// has to be kept in step with this one.
	Objects []Placed `json:"-"`

	Format string `json:"_format"`

	// Archive, Plan, Generation and Volume bind this file to one cycle. A
	// checkpoint is a third thing that can be crossed with a plan and a
	// catalog, and crossing those was unrecoverable (DESIGN §10.2), so all four
	// are checked before a single record is taken on trust.
	Archive    string `json:"archive"`
	Plan       string `json:"plan,omitempty"`
	Generation int    `json:"generation"`
	Volume     string `json:"volume"`

	Paused string `json:"paused"`           // RFC3339, for the operator
	Reason string `json:"reason,omitempty"` // what asked for the stop

	// Problems carries forward the source files this run could not read. They
	// are not on the medium and must not be silently forgotten by the resume:
	// the volume's final report is the one place the operator learns that the
	// next cycle owes them a write.
	Problems []CheckpointProblem `json:"problems,omitempty"`

	// Remaining is how many plan entries were still to go, for the report
	// alone. Nothing is decided from it — the plan is the authority on what is
	// left, and it is re-read on the resume.
	Remaining int `json:"remaining,omitempty"`

	// Count is how many objects the stream after this header holds. It is set by
	// SaveCheckpoint from the slice itself, so it cannot drift, and it earns its
	// place twice: ReadCheckpointHeader can answer "how much is on that
	// cartridge?" without decompressing 20 MB, and a stream that comes up short
	// of it is a truncated file rather than a shorter pause.
	Count int `json:"objects,omitempty"`
}

// Placed is one object on the medium, with everything a resume needs to adopt it
// without reading it back.
type Placed struct {
	// Object is the name on the medium; Stored is what it occupies there, which
	// for a container is the encoded tar rather than the sum of its members.
	Object string `json:"object"`
	Stored int64  `json:"stored"`

	// Records is what this object contributed to the catalog: one for a data
	// object or a fragment, one per member for a container.
	Records []*catalog.Record `json:"records"`

	// Sources is the files this object was built from, as they were when they
	// were read. The host may have been off for a week between the pause and
	// the resume, so a re-stat against these is what stops a record describing
	// bytes that are no longer what the path holds — the hazard a re-read of
	// the medium cannot see, because it compares the medium against itself.
	Sources []Source `json:"sources"`
}

// Source is a file's identity at the moment the writer read it.
//
// Ctime and inode may be zero on a filesystem that does not give them, and a
// zero is a check that does not run rather than one that fails — the same
// degradation catalog.Record makes for the same reason.
type Source struct {
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	CTime int64  `json:"ctime,omitempty"`
	Inode uint64 `json:"inode,omitempty"`
}

// CheckpointProblem is a source file a paused run could not store.
type CheckpointProblem struct {
	Path string `json:"path"`
	Err  string `json:"err"`
}

// sourceOf records a file's identity from the stat the writer already did.
func sourceOf(path string, info os.FileInfo) Source {
	s := Source{Path: path, Size: info.Size()}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		s.CTime = st.Ctim.Nano()
		s.Inode = st.Ino
	}
	return s
}

// unchanged reports whether the file is still the one that was read.
func (s Source) unchanged() error {
	info, err := os.Stat(s.Path)
	if err != nil {
		return err
	}
	if info.Size() != s.Size {
		return fmt.Errorf("%d bytes now, %d when it was written", info.Size(), s.Size)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if s.Inode != 0 && st.Ino != s.Inode {
		return fmt.Errorf("inode %d now, %d when it was written", st.Ino, s.Inode)
	}
	// The nanosecond ctime is the whole point: a file rewritten at the same
	// size with its mtime put back is indistinguishable from an untouched one
	// by every other field. DESIGN §6.
	if s.CTime != 0 && st.Ctim.Nano() != s.CTime {
		return errors.New("changed since it was written")
	}
	return nil
}

// LoadCheckpoint reads a paused write's checkpoint. A missing file is not an
// error: it is the ordinary case, and it means there is nothing to resume from.
//
// Decoded with json.Decoder rather than scanned line by line, because one line
// can be a container's whole membership — thousands of records for a single
// object — and a scanner would need a token limit chosen against the largest
// container anyone ever configures.
func LoadCheckpoint(path string) (*Checkpoint, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("%s: gzip: %w", path, err)
		}
		defer gz.Close()
		r = gz
	}

	cp := new(Checkpoint)
	dec := json.NewDecoder(r)
	if err := dec.Decode(cp); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if cp.Format != ResumeFormat {
		return nil, fmt.Errorf("%s is format %q, this build writes %q", path, cp.Format, ResumeFormat)
	}
	for {
		var pl Placed
		switch err := dec.Decode(&pl); {
		case errors.Is(err, io.EOF):
			if cp.Count != len(cp.Objects) {
				// Truncated — a full disk during the pause, most likely. The
				// objects it does carry would each still be gated, so adopting a
				// prefix would be safe; refusing is better anyway, because a file
				// that lost its tail is a file whose middle is also in doubt, and
				// the crash path is what the caller falls back to.
				return nil, fmt.Errorf("%s claims %d objects and carries %d: it is truncated",
					path, cp.Count, len(cp.Objects))
			}
			return cp, nil
		case err != nil:
			return nil, fmt.Errorf("%s, object %d: %w", path, len(cp.Objects)+1, err)
		}
		cp.Objects = append(cp.Objects, pl)
	}
}

// ReadCheckpointHeader reads only the first line: the cycle a pause belongs to,
// when it happened, and how much it placed.
//
// It exists for the same reason catalog.ReadHeader does. Three callers — the
// pause report, the plan's part-written test, and the retirement of an abandoned
// cycle — want nothing but those fields, and decoding the stream to get them
// means decompressing an entry per file, which on the real archive's largest
// volume is 20 MB and half a million records.
func ReadCheckpointHeader(path string) (*Checkpoint, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("%s: gzip: %w", path, err)
		}
		defer gz.Close()
		r = gz
	}
	cp := new(Checkpoint)
	if err := json.NewDecoder(r).Decode(cp); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if cp.Format != ResumeFormat {
		return nil, fmt.Errorf("%s is format %q, this build writes %q", path, cp.Format, ResumeFormat)
	}
	return cp, nil
}

// SaveCheckpoint writes a checkpoint durably.
//
// This is the one local file that is fsynced, and for the one reason worth paying
// for it: the pause it records exists so the host can be powered off, so a
// checkpoint sitting in the page cache would be a promise kept only for as long
// as nothing went wrong. The directory is synced too, or the rename that made the
// file visible could itself be lost.
//
// Gzipped JSONL, for the same reason the catalog is (catalog.Write): a checkpoint
// holds an entry per file, and the real archive's largest volume holds 539,275 of
// them. Both figures here are measured against that volume's own paths rather
// than estimated. One indented JSON object costs 1,247 bytes an entry — 673 MB,
// allocated whole in memory and fsynced, at every pause. A header line plus one
// line per object, gzipped and streamed, costs 38: 20.5 MB for the same volume, a
// 33x reduction. Most of that is gzip absorbing the paths, which appear twice per
// entry because a source stamp and a record both name the file — which is why
// that duplication is left alone rather than engineered away.
func SaveCheckpoint(path string, cp *Checkpoint) (err error) {
	cp.Format = ResumeFormat
	cp.Count = len(cp.Objects)
	dir := filepath.Dir(path)
	// CreateTemp's 0600 is kept deliberately, as in catalog.Write: a checkpoint
	// names every path it vouches for, which in encrypted mode is precisely what
	// the volumes themselves do not reveal (DESIGN §3.2).
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tmp.Close() // already closed on the success path; the error is moot here
			os.Remove(tmp.Name())
		}
	}()

	bw := bufio.NewWriterSize(tmp, 1<<20)
	var w io.Writer = bw
	var gz *gzip.Writer
	if strings.HasSuffix(path, ".gz") {
		gz = gzip.NewWriter(bw)
		w = gz
	}
	enc := json.NewEncoder(w)
	if err = enc.Encode(cp); err != nil {
		return err
	}
	for i := range cp.Objects {
		if err = enc.Encode(&cp.Objects[i]); err != nil {
			return err
		}
	}
	if gz != nil {
		if err = gz.Close(); err != nil {
			return err
		}
	}
	if err = bw.Flush(); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	// Close reports write errors a buffered filesystem held back until now, so it
	// is checked rather than deferred away.
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(dir)
}

// syncDir makes a rename durable. Without it the file's contents survive a power
// cut and the name that reaches them does not.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// RemoveCheckpoint drops the checkpoint once the volume it describes is finished
// and its records are in the catalog. A missing one is success.
func RemoveCheckpoint(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Describes reports whether this checkpoint is about the volume a write is
// about to place, which is what lets a caller leave another volume's pause
// alone. A pause on volume 3 must survive the operator writing volume 5.
func (cp *Checkpoint) Describes(archiveID, planID string, gen int, volumeID string) bool {
	if cp == nil {
		return false
	}
	return cp.Archive == archiveID && cp.Generation == gen && cp.Volume == volumeID &&
		(cp.Plan == "" || planID == "" || cp.Plan == planID)
}

// adoptObjects decides which of a checkpoint's objects a resume may take on
// trust, and says why it refused the rest.
//
// The cycle gates are the caller's (Describes), and they are all-or-nothing: a
// checkpoint for another archive, plan, generation or volume is not stale, it is
// about something else, and nothing in it applies. Past those, each object
// stands on its own: a changed source, or a length the medium disagrees with,
// costs that one object its adoption and leaves the others untouched. A refused
// object is not written and not read back — the writer reports the file and
// records nothing for it, because the name on the medium is taken and there is
// no second one to put a corrected copy under. DESIGN §11.1.
func adoptObjects(cp *Checkpoint, volumeID string, present map[string]int64) (adopted map[string]Placed, refused map[string]string) {
	adopted = make(map[string]Placed, len(cp.Objects))
	refused = make(map[string]string)
	for _, pl := range cp.Objects {
		size, onMedium := present[pl.Object]
		switch {
		case !onMedium:
			// Put is atomic, so absent means it never landed rather than that
			// it landed badly. The plan writes it again under the same name.
			continue
		case size != pl.Stored:
			refused[pl.Object] = fmt.Sprintf("%d bytes on %s, the pause recorded %d", size, volumeID, pl.Stored)
			continue
		}
		if bad := changedSource(pl.Sources); bad != "" {
			refused[pl.Object] = bad
			continue
		}
		adopted[pl.Object] = pl
	}
	return adopted, refused
}

// changedSource returns a description of the first source that has moved under
// us, or "" when every one of them is still what was read.
func changedSource(sources []Source) string {
	for _, s := range sources {
		if err := s.unchanged(); err != nil {
			return fmt.Sprintf("%s %v", s.Path, err)
		}
	}
	return ""
}

// checkpoint assembles what the caller has to persist for this pause. Reason is
// left for the caller to fill, since only it knows whether a signal or the flag
// in the workdir asked for the stop.
func (w *writer) checkpoint(remaining int) *Checkpoint {
	cp := &Checkpoint{
		Format:     ResumeFormat,
		Archive:    w.opt.Archive,
		Plan:       w.opt.Plan,
		Generation: w.opt.Generation,
		Volume:     w.id,
		Paused:     time.Now().UTC().Format(time.RFC3339),
		Objects:    w.placed,
		Remaining:  remaining,
	}
	for _, p := range w.res.Problems {
		cp.Problems = append(cp.Problems, CheckpointProblem{Path: p.Path, Err: p.Err.Error()})
	}
	return cp
}
