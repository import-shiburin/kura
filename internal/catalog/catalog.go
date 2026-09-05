// Package catalog is the archive's memory: every version of every path ever
// written, which volume holds the bytes, and whether those bytes can still be
// read.
//
// It is a history rather than a snapshot. Space is never reclaimed, so the
// bytes for every past state stay on the media whether or not anything
// remembers them — only the bookkeeping is optional, and it costs one line per
// change. Reconstructing the tree as of any past generation is then a query
// against records that were never removed. See DESIGN.md §5.
package catalog

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Format is written into the header line of every catalog. Bump it only for
// changes a reader of the previous version could not survive.
const Format = "kura-catalog/1"

// Hash kinds, as they appear in Record.HashKind. They name values in the
// on-media format, so they live with the format rather than with the code that
// computes the digests.
const (
	HashKindSample = "sample-1m"
	HashKindFull   = "full"
)

// Record is one version of one path as recorded in the catalog.
//
// Field names are spelled out rather than abbreviated on purpose: a copy of
// this catalog is written to every volume as the authoritative index, and it
// has to stay readable years from now by someone with nothing but a text
// editor.
type Record struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime"`

	// CTime (nanoseconds) and Inode back up the mtime check. A program that
	// rewrites a file and restores its mtime leaves size and mtime identical,
	// and mtime alone would call that unchanged and silently never back it up
	// again. ModTime is stored at second granularity; the nanosecond ctime is
	// what covers a change made within the same second as the last scan.
	//
	// Zero means the record predates these fields, and the check degrades to
	// mtime rather than flagging every file.
	CTime int64  `json:"ctime,omitempty"`
	Inode uint64 `json:"inode,omitempty"`

	Mode     uint32 `json:"mode,omitempty"`
	Link     string `json:"link,omitempty"`     // symlink target, if any
	HardLink string `json:"hardlink,omitempty"` // path of the record holding the data
	Hash     string `json:"hash,omitempty"`
	HashKind string `json:"hash_kind,omitempty"`
	FullHash string `json:"full_hash,omitempty"` // whole-file digest, from the writer

	// Volume is the medium the bytes were written to.
	Volume string `json:"volume,omitempty"`

	// Object is the name the bytes are stored under on Volume, when that is not
	// Path. It differs when a file was moved and the catalog now points a new
	// name at data written under the old one, and it differs for everything in
	// an encrypted archive, where objects carry random IDs (DESIGN §3.2).
	Object string `json:"object,omitempty"`

	// Container names the object holding these bytes when the file was bundled
	// into a tar container rather than written as an object of its own, in
	// which case Object is the member name inside it.
	//
	// Small files are containerised to stop the filesystem charging a per-file
	// cost — 20-50 ms each on LTFS, which across the 1.44 M files under 1 MB in
	// the real archive is the difference between a workable write and a day of
	// waiting (DESIGN §4). The container is an ordinary tar, so a volume stays
	// readable with nothing but `tar`.
	Container string `json:"container,omitempty"`

	// Cipher names the algorithm these bytes were encrypted with; empty means
	// plaintext.
	//
	// It is recorded per object rather than read from the volume header because
	// reclamation stages ciphertext verbatim (DESIGN §9.1): an object moved to
	// a newer volume keeps the algorithm it was written under, and its new
	// home's header describes the volume's current default instead. Reading
	// therefore dispatches on this field, and an empty one never means
	// "whatever the volume says" — that ambiguity is what the field exists to
	// remove.
	//
	// It names an algorithm and not a key, which is enough only while every
	// volume in an archive derives the same key. DESIGN §3.1 puts the KDF salt
	// in each volume header without saying whether it is per archive or per
	// volume; if it is per volume, a migrated object also needs to name the
	// header its key came from, and that is another field here.
	Cipher string `json:"cipher,omitempty"`

	// Gen is the generation that wrote this version. DeletedGen is the
	// generation in which the path stopped existing on disk; zero means it is
	// still live.
	//
	// Records are never removed, only tombstoned. LTFS cannot reclaim the space
	// a deleted file occupied, so the bytes stay on the volume whether or not
	// anything remembers them. Keeping the tombstone costs a catalog line and is
	// the difference between "we have that data somewhere" and being able to
	// actually find it.
	Gen        int `json:"gen,omitempty"`
	DeletedGen int `json:"deleted_gen,omitempty"`

	// Lost marks a version whose bytes are no longer retrievable. Nil is the
	// ordinary case.
	Lost *Lost `json:"lost,omitempty"`

	Parts []Part `json:"parts,omitempty"` // set when the file is split across volumes
}

// LostReason says why a version's bytes are gone.
//
// The two are never collapsed into one flag: reclaimed was chosen, media was
// suffered, and whoever reads the report needs to know which. They also differ
// in reversibility. See DESIGN §5.1.
type LostReason string

const (
	// LostMedia is a current assessment — the object failed to read. It can be
	// cleared if a later attempt, on another drive, succeeds.
	LostMedia LostReason = "media"

	// LostReclaimed is permanent: the volume was erased on purpose.
	LostReclaimed LostReason = "reclaimed"
)

// Lost records that a version's bytes are no longer retrievable.
//
// The marker sits on the version rather than the path, because a record already
// covers a generation range. A file written at generation 1 and rewritten at 5
// has two records on two volumes; damage to the older volume makes the file
// unavailable for generations 1–4 and fine from 5 onward, and marking the
// record gives that for free.
//
// A lost record is never removed. It still carries the size, the digest, the
// generations it covered and the volume it was on — which is what lets a
// point-in-time query report "1,247 objects unavailable" instead of quietly
// returning a short tree, and what lets the content be recognised if it ever
// turns up again.
type Lost struct {
	Gen    int        `json:"gen"`              // the generation that recorded the loss
	Reason LostReason `json:"reason"`           // media | reclaimed
	Detail string     `json:"detail,omitempty"` // "LTO007 unreadable during verify 2026-09-03"
}

// Part is one fragment of a split file.
//
// Each fragment carries its own object name and digest rather than borrowing
// the record's, because verification is per volume: the whole point of the
// workflow is that one cartridge can be checked on its own, weeks apart from
// the one holding the rest of the file.
type Part struct {
	Volume string `json:"volume"`
	Object string `json:"object,omitempty"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
	Hash   string `json:"hash,omitempty"` // digest of this fragment alone
}

// Tombstoned reports whether this version was explicitly marked as gone.
//
// It is not the same as "superseded": when a file is edited, the older version
// keeps DeletedGen zero, because its bytes are still on the volume and still the
// correct answer for any generation before the edit. Whether a path exists is
// therefore a property of its newest version, not of any one record.
func (r *Record) Tombstoned() bool { return r.DeletedGen != 0 }

// GoneBy reports whether this version had been deleted as of generation g.
func (r *Record) GoneBy(g int) bool { return r.DeletedGen != 0 && r.DeletedGen <= g }

// IsLost reports whether these bytes are currently unretrievable.
//
// Diff treats a lost version as needing a write even when the file's stat data
// is unchanged, so marking something lost re-protects it on the next cycle for
// anything still on the source. That self-healing is the main reason the marker
// exists, and it falls out of the existing incremental logic rather than needing
// machinery of its own (DESIGN §5.1).
func (r *Record) IsLost() bool { return r.Lost != nil }

// MarkLost records a loss.
//
// A repeat media finding does not overwrite the first one — the original detail
// dates the discovery, which is what a degradation trend needs. Reclamation does
// overwrite, because it is both permanent and a different kind of fact from a
// failed read.
func (r *Record) MarkLost(l Lost) {
	if r.Lost == nil || l.Reason == LostReclaimed {
		r.Lost = &l
	}
}

// ClearLost removes a media marker after the object read successfully — a
// retry, a different drive, a volume that turned up. Clearing an unmarked
// record is not an error.
//
// It refuses to clear a reclamation. That is not an assessment open to
// revision: the bytes were erased deliberately, and a successful read of
// something at that name would mean the catalog has lost track of what it
// stored.
func (r *Record) ClearLost() error {
	if r.Lost == nil {
		return nil
	}
	if r.Lost.Reason == LostReclaimed {
		return fmt.Errorf("%s at generation %d was reclaimed, not damaged: those bytes were erased on purpose", r.Path, r.Gen)
	}
	r.Lost = nil
	return nil
}

// Inside reports whether the bytes are a member of a container rather than an
// object of their own.
func (r *Record) Inside() bool { return r.Container != "" }

// ReadFrom is the object a restore has to ask the volume for: the container
// when the file was bundled, otherwise the object itself.
func (r *Record) ReadFrom() string {
	if r.Container != "" {
		return r.Container
	}
	return r.ObjectName()
}

// ObjectName is the name the bytes were stored under, which is what a restore
// has to ask the volume for.
func (r *Record) ObjectName() string {
	if r.Object != "" {
		return r.Object
	}
	return r.Path
}

// Volumes lists every volume holding part of this record, first appearance
// first.
//
// A split file lives on more than one, which is why damage to a single volume
// cannot be decided by comparing Volume alone. Both fields are consulted rather
// than treating Parts as authoritative when present: under-reporting which
// volumes a record depends on would leave damage unmarked.
func (r *Record) Volumes() []string {
	out := make([]string, 0, 1+len(r.Parts))
	seen := make(map[string]bool, 1+len(r.Parts))
	add := func(v string) {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	add(r.Volume)
	for _, p := range r.Parts {
		add(p.Volume)
	}
	return out
}

// UsesVolume reports whether any of this record's bytes are on the named
// volume, without building a slice per record the way Volumes does — this runs
// once per record over a catalog with millions of them.
//
// An empty id matches nothing: a record with no volume has not been written
// yet, and reporting those as damaged would be a wrong answer in the one query
// that decides what to re-source.
func (r *Record) UsesVolume(id string) bool {
	if id == "" {
		return false
	}
	if r.Volume == id {
		return true
	}
	for _, p := range r.Parts {
		if p.Volume == id {
			return true
		}
	}
	return false
}

// Header is the first line of a catalog file.
type Header struct {
	Format string `json:"_format"`

	// Archive identifies this archive across every volume it owns. A volume
	// whose header names a different archive is refused rather than written to,
	// which is the "foreign" row of the reconciliation table (DESIGN §11).
	Archive string `json:"archive,omitempty"`

	Generation int    `json:"generation"`
	Written    string `json:"written,omitempty"`
	Note       string `json:"note,omitempty"`

	// Timeline dates every generation, so a restore can be asked for the tree
	// as it stood on a date rather than at a generation number.
	Timeline []GenerationInfo `json:"timeline,omitempty"`

	// Volumes is the inventory: what exists and when each was last checked.
	// A full scrub reads every byte and cannot be one command that runs for
	// days, so the tool has to be able to answer "which is most overdue?"
	// (DESIGN §8).
	Volumes []VolumeInfo `json:"volumes,omitempty"`
}

// VolumeInfo is what the catalog remembers about one volume.
type VolumeInfo struct {
	ID           string `json:"id"`
	Written      string `json:"written,omitempty"`
	LastVerified string `json:"last_verified,omitempty"`
	Objects      int64  `json:"objects,omitempty"`
	Bytes        int64  `json:"bytes,omitempty"`
}

// Volume returns the inventory entry for a volume, creating it if this is the
// first time the archive has seen that ID.
func (h *Header) Volume(id string) *VolumeInfo {
	for i := range h.Volumes {
		if h.Volumes[i].ID == id {
			return &h.Volumes[i]
		}
	}
	h.Volumes = append(h.Volumes, VolumeInfo{ID: id})
	return &h.Volumes[len(h.Volumes)-1]
}

// GenerationInfo dates one cycle and names the volumes it wrote.
type GenerationInfo struct {
	Gen     int      `json:"gen"`
	Written string   `json:"written"`
	Volumes []string `json:"volumes,omitempty"`
	Note    string   `json:"note,omitempty"`
}

// Catalog is the in-memory view of everything ever written to media.
type Catalog struct {
	Header  Header
	Records []*Record
	byPath  map[string][]*Record // every version of a path, generation-ascending
}

// New returns an empty catalog, as the first generation starts from.
func New() *Catalog { return &Catalog{byPath: make(map[string][]*Record)} }

// Add records new versions.
//
// Records normally arrive with the newest generation last, so this is an
// append; the insertion search covers the case where they do not, because every
// query in this package reads a path's version list as generation-ascending and
// would otherwise answer from the wrong version.
//
// The fragments of a split file arrive one at a time, because each volume is
// written by its own invocation days apart. They are merged into the single
// record for that path and generation rather than accumulating one record per
// cartridge: a path has one version per generation, and a restore has to be
// able to see the whole file from it.
func (c *Catalog) Add(recs ...*Record) {
	for _, r := range recs {
		if len(r.Parts) > 0 {
			if into := c.version(r.Path, r.Gen); into != nil && len(into.Parts) > 0 {
				into.mergeParts(r.Parts)
				continue
			}
		}
		c.Records = append(c.Records, r)
		versions := c.byPath[r.Path]
		i := sort.Search(len(versions), func(i int) bool { return versions[i].Gen > r.Gen })
		versions = append(versions, nil)
		copy(versions[i+1:], versions[i:])
		versions[i] = r
		c.byPath[r.Path] = versions
	}
}

// version returns the record for a path at exactly generation g, if there is
// one.
func (c *Catalog) version(path string, g int) *Record {
	for _, v := range c.byPath[path] {
		if v.Gen == g {
			return v
		}
	}
	return nil
}

// mergeParts folds another volume's fragments into this record, in file order.
//
// A fragment already present at the same offset is replaced rather than
// duplicated: a resumed write re-reports the pieces it recognises, and two
// entries for one byte range would make the file look longer than it is.
func (r *Record) mergeParts(parts []Part) {
	for _, p := range parts {
		replaced := false
		for i := range r.Parts {
			if r.Parts[i].Offset == p.Offset {
				r.Parts[i] = p
				replaced = true
				break
			}
		}
		if !replaced {
			r.Parts = append(r.Parts, p)
		}
	}
	sort.Slice(r.Parts, func(i, j int) bool { return r.Parts[i].Offset < r.Parts[j].Offset })
}

// newest returns the most recent version of a path written at or before g.
// Pass 0 for g to mean "no limit".
func (c *Catalog) newest(path string, g int) *Record {
	versions := c.byPath[path] // generation-ascending
	for i := len(versions) - 1; i >= 0; i-- {
		if g == 0 || versions[i].Gen <= g {
			return versions[i]
		}
	}
	return nil
}

// Lookup returns the current version of a path, if the path still exists.
//
// A lost version is still returned: the path does exist, it just cannot
// currently be produced. Callers check IsLost themselves, because what to do
// about it differs — Diff rewrites, restore fails loudly, a report counts.
func (c *Catalog) Lookup(path string) (*Record, bool) {
	r := c.newest(path, 0)
	if r == nil || r.Tombstoned() {
		return nil, false
	}
	return r, true
}

// Versions returns every recorded version of a path, oldest first.
func (c *Catalog) Versions(path string) []*Record { return c.byPath[path] }

// LiveRecords returns one record per path that still exists on disk.
//
// Deletion detection has to run over these rather than over every record: a
// file removed three cycles ago would otherwise be reported as newly deleted
// forever, and a superseded version would be mistaken for a second live file.
func (c *Catalog) LiveRecords() []*Record {
	out := make([]*Record, 0, len(c.byPath))
	for path := range c.byPath {
		if r, ok := c.Lookup(path); ok {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// At reconstructs the tree as it stood at the end of generation g: for every
// path, the newest version written at or before g that had not yet been
// deleted by g.
//
// This is the whole payoff of never reclaiming space. The bytes for every one
// of these records are still on their original volume, so any past state of
// the archive is restorable from what is already on the shelf.
//
// Lost versions are returned flagged rather than omitted, so the caller can be
// honest about the holes. The loss is not compared against g: a marker records
// that the bytes are gone now, which is equally true of a query about the past.
// Lost.Gen dates the discovery for the report, and does not filter.
func (c *Catalog) At(g int) []*Record {
	out := make([]*Record, 0, len(c.byPath))
	for path := range c.byPath {
		// The newest version as of g, then a separate check for whether that
		// version had been deleted by then. Doing it in one pass would let an
		// older version resurface a path that a later generation removed.
		r := c.newest(path, g)
		if r != nil && !r.GoneBy(g) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// OnVolume returns every record with bytes on the named volume, including
// superseded and tombstoned versions.
//
// Those are the point. When a volume is damaged or reclaimed, what becomes
// unavailable is every version it held, not only the ones that happen to be
// current: a generation-1 record whose path was rewritten at generation 5 is
// still the correct answer for a restore of generation 3.
func (c *Catalog) OnVolume(id string) []*Record {
	var out []*Record
	for _, r := range c.Records {
		if r.UsesVolume(id) {
			out = append(out, r)
		}
	}
	sortRecords(out)
	return out
}

// MaxGen returns the highest generation number present in the catalog.
func (c *Catalog) MaxGen() int {
	g := c.Header.Generation
	for _, r := range c.Records {
		if r.Gen > g {
			g = r.Gen
		}
	}
	return g
}

// index rebuilds the per-path version lists.
func (c *Catalog) index() {
	c.byPath = make(map[string][]*Record, len(c.Records))
	for _, r := range c.Records {
		c.byPath[r.Path] = append(c.byPath[r.Path], r)
	}
	for _, versions := range c.byPath {
		sort.Slice(versions, func(i, j int) bool { return versions[i].Gen < versions[j].Gen })
	}
}

// Load reads a JSONL catalog, transparently handling gzip. A missing file is
// not an error: it means this is the first generation.
func Load(path string) (*Catalog, error) {
	c := New()
	if path == "" {
		return c, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, err
	}
	defer f.Close()

	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		defer gz.Close()
		r = gz
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		b := sc.Bytes()
		if len(b) == 0 {
			continue
		}
		if line == 1 && looksLikeHeader(b) {
			if err := json.Unmarshal(b, &c.Header); err != nil {
				return nil, fmt.Errorf("catalog header: %w", err)
			}
			if c.Header.Format != Format {
				return nil, fmt.Errorf("catalog format %q, this build understands %q", c.Header.Format, Format)
			}
			continue
		}
		rec := new(Record)
		if err := json.Unmarshal(b, rec); err != nil {
			return nil, fmt.Errorf("catalog line %d: %w", line, err)
		}
		c.Records = append(c.Records, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	c.index()
	return c, nil
}

func looksLikeHeader(b []byte) bool {
	var probe struct {
		Format string `json:"_format"`
	}
	return json.Unmarshal(b, &probe) == nil && probe.Format != ""
}

// Write emits a catalog as JSONL, gzipping when the path ends in .gz.
//
// The same bytes go to disk as working state and onto every volume as the
// authoritative index, so the header is written first and the records follow in
// path order: the file stays greppable and diffable by hand.
//
// It writes a temporary file alongside the target and renames it into place. A
// crash or a full disk therefore leaves the previous catalog intact rather than
// a truncated one — this file is the only record of where 31 TB of bytes live,
// and a half-finished write would strand every volume on the shelf.
func Write(path string, h Header, recs []*Record) (err error) {
	dir := filepath.Dir(path)

	// CreateTemp's 0600 is kept deliberately. The catalog lists every path in
	// the archive, which in encrypted mode is precisely what the volumes
	// themselves do not reveal (DESIGN §3.2).
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

	h.Format = Format
	enc := json.NewEncoder(w)
	if err = enc.Encode(h); err != nil {
		return err
	}
	sorted := append([]*Record(nil), recs...)
	sortRecords(sorted)
	for _, r := range sorted {
		if err = enc.Encode(r); err != nil {
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
	// One fsync per cycle, not the per-object fsync the writer deliberately
	// avoids: renaming onto a file whose contents have not reached the platter
	// is not durable, and the whole point of the rename is to survive a crash.
	if err = tmp.Sync(); err != nil {
		return err
	}
	// Close reports write errors that a buffered filesystem held back until now,
	// so it is checked rather than deferred away.
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(dir)
}

// syncDir makes the rename itself durable; without it the new catalog can be on
// the platter while the directory entry still points at the old one.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// sortRecords orders by path, then by generation, which is the order a catalog
// is written and read back in.
func sortRecords(recs []*Record) {
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].Path != recs[j].Path {
			return recs[i].Path < recs[j].Path
		}
		return recs[i].Gen < recs[j].Gen
	})
}
