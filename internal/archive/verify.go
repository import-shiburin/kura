package archive

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"kura/internal/catalog"
	"kura/internal/scan"
	"kura/internal/volume"
)

// Finding is one object that did not read back as recorded.
type Finding struct {
	Path   string
	Object string
	Err    error
}

// VerifyResult is what one volume's scrub found.
type VerifyResult struct {
	Volume  string
	Checked int
	Bytes   int64

	// Elapsed and Bytes together give the throughput figure that channel 3 of
	// the degradation signals depends on. A drive re-reading marginal media
	// internally delivers data more slowly, and that slowdown is the only sign
	// of corrections the filesystem hides — but only if it is collected from
	// the very first scrub, since a trend started late is worthless (DESIGN
	// §8.1).
	Elapsed time.Duration

	Findings []Finding

	// Unclaimed lists objects present on the volume that no record accounts
	// for. They are reported rather than deleted: on media that never reclaims
	// space, an object nothing remembers is a sign the catalog lost track of a
	// write, and the answer is to look, not to tidy.
	Unclaimed []string
}

// Failed reports whether anything did not read back as recorded.
func (r *VerifyResult) Failed() bool { return len(r.Findings) > 0 }

// Verify reads every object this volume is recorded as holding and checks it
// against the digest the writer stored.
//
// This is the only tier that catches actual bit rot, and it is the expensive
// one — a full volume is hours. Nothing below us reports a correction, so a
// read either succeeds, in which case we cannot tell how hard the drive worked,
// or it fails outright (DESIGN §8.1). Detect and report; never repair.
//
// Records whose objects cannot be produced are marked lost, which is what makes
// the next cycle re-source them from the live tree without any extra machinery.
func Verify(ctx context.Context, vol volume.Volume, cat *catalog.Catalog, mark bool) (*VerifyResult, error) {
	if _, err := ReadHeader(ctx, vol); err != nil {
		return nil, err
	}
	started := time.Now()
	res := &VerifyResult{Volume: vol.ID()}

	records := cat.OnVolume(vol.ID())
	// Both plaintext metadata objects are accounted for by existing rather than
	// by a catalog record; an unclaimed report on them would be noise on every
	// single verify.
	claimed := map[string]bool{HeaderObject: true, ArchiveObject: true}

	// Group container members so each container is read once, in one pass,
	// rather than once per member. On tape that is the difference between a
	// sequential read and a seek per file.
	byContainer := make(map[string][]*catalog.Record)
	for _, r := range records {
		switch {
		case r.Inside():
			byContainer[r.Container] = append(byContainer[r.Container], r)
		case len(r.Parts) > 0:
			res.checkParts(ctx, vol, r, claimed)
		default:
			res.checkObject(ctx, vol, r, claimed)
		}
	}
	for name, members := range byContainer {
		res.checkContainer(ctx, vol, name, members, claimed)
	}

	objs, err := vol.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, o := range objs {
		if !claimed[o.Name] && !isGenerationObject(o.Name) {
			res.Unclaimed = append(res.Unclaimed, o.Name)
		}
	}

	res.Elapsed = time.Since(started)
	if mark {
		res.markLost(cat)
		info := cat.Header.Volume(vol.ID())
		// Only a clean pass updates the timestamp. A volume with findings has
		// not been verified; it has been found wanting, and it should still
		// come up as overdue until it reads clean or is migrated.
		if !res.Failed() {
			info.LastVerified = time.Now().UTC().Format(time.RFC3339)
		}
	}
	return res, nil
}

func (r *VerifyResult) checkObject(ctx context.Context, vol volume.Volume, rec *catalog.Record, claimed map[string]bool) {
	name := rec.ObjectName()
	claimed[name] = true
	_, full, err := digestObject(ctx, vol, name, rec.Size)
	if err != nil {
		r.Findings = append(r.Findings, Finding{Path: rec.Path, Object: name, Err: err})
		return
	}
	r.Checked++
	r.Bytes += rec.Size
	if rec.FullHash != "" && full != rec.FullHash {
		r.Findings = append(r.Findings, Finding{
			Path: rec.Path, Object: name,
			Err: errors.New("digest mismatch: the object read back is not what was written"),
		})
	}
}

func (r *VerifyResult) checkParts(ctx context.Context, vol volume.Volume, rec *catalog.Record, claimed map[string]bool) {
	for _, p := range rec.Parts {
		if p.Volume != vol.ID() {
			continue // another cartridge's problem, checked when it is mounted
		}
		claimed[p.Object] = true
		_, full, err := digestObject(ctx, vol, p.Object, p.Length)
		if err != nil {
			r.Findings = append(r.Findings, Finding{Path: rec.Path, Object: p.Object, Err: err})
			continue
		}
		r.Checked++
		r.Bytes += p.Length
		if p.Hash != "" && full != p.Hash {
			r.Findings = append(r.Findings, Finding{
				Path: rec.Path, Object: p.Object,
				Err: errors.New("digest mismatch on a fragment"),
			})
		}
	}
}

func (r *VerifyResult) checkContainer(ctx context.Context, vol volume.Volume, name string, members []*catalog.Record, claimed map[string]bool) {
	claimed[name] = true
	digests, err := digestContainer(ctx, vol, name)
	if err != nil {
		// One unreadable container costs every file in it. That is the blast
		// radius containers trade away for write speed, and the report has to
		// name each casualty rather than the container alone.
		for _, m := range members {
			r.Findings = append(r.Findings, Finding{Path: m.Path, Object: name, Err: err})
		}
		return
	}
	for _, m := range members {
		d, ok := digests[m.ObjectName()]
		if !ok {
			r.Findings = append(r.Findings, Finding{
				Path: m.Path, Object: name,
				Err: fmt.Errorf("container does not hold member %q", m.ObjectName()),
			})
			continue
		}
		r.Checked++
		r.Bytes += m.Size
		if m.FullHash != "" && d.full != m.FullHash {
			r.Findings = append(r.Findings, Finding{
				Path: m.Path, Object: name,
				Err: errors.New("digest mismatch inside the container"),
			})
		}
	}
}

// markLost records what could not be read, so the next cycle re-protects it.
func (r *VerifyResult) markLost(cat *catalog.Catalog) {
	byPath := make(map[string][]*catalog.Record)
	for _, rec := range cat.OnVolume(r.Volume) {
		byPath[rec.Path] = append(byPath[rec.Path], rec)
	}
	gen := cat.MaxGen()
	for _, f := range r.Findings {
		for _, rec := range byPath[f.Path] {
			rec.MarkLost(catalog.Lost{
				Gen:    gen,
				Reason: catalog.LostMedia,
				Detail: fmt.Sprintf("%s on %s: %v", f.Object, r.Volume, f.Err),
			})
		}
	}
}

// digestObject reads one object and returns its sampled and whole-file digests.
func digestObject(ctx context.Context, vol volume.Volume, name string, size int64) (sampled, full string, err error) {
	rc, err := vol.Get(ctx, name)
	if err != nil {
		return "", "", err
	}
	defer rc.Close()

	h := scan.NewHasher(size)
	if _, err := io.Copy(h, rc); err != nil {
		return "", "", err
	}
	return h.Sums()
}

type memberDigest struct{ sampled, full string }

// digestContainer reads a container once and digests every member.
func digestContainer(ctx context.Context, vol volume.Volume, name string) (map[string]memberDigest, error) {
	rc, err := vol.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	out := make(map[string]memberDigest)
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		h := scan.NewHasher(hdr.Size)
		if _, err := io.Copy(h, tr); err != nil {
			return nil, err
		}
		sampled, full, err := h.Sums()
		if err != nil {
			return nil, fmt.Errorf("member %s: %w", hdr.Name, err)
		}
		out[hdr.Name] = memberDigest{sampled, full}
	}
}
