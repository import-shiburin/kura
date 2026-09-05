package archive

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"kura/internal/volume"
)

// HeaderMagic marks an object as a kura volume header.
const HeaderMagic = "kura-volume"

// ArchiveMagic marks an object as a kura archive header. Distinct from
// HeaderMagic so the two plaintext objects cannot be mistaken for each other by
// a reader that finds only one of them.
const ArchiveMagic = "kura-archive"

// HeaderFormat versions the header itself. It is deliberately separate from the
// catalog and plan formats: this is the one structure a future reader must be
// able to parse before it knows anything else about the volume.
const HeaderFormat = 1

// ArchiveHeader is what is true of the archive rather than of one cartridge
// (DESIGN §3.1): its identity, and how its objects are encrypted.
//
// Split out from VolumeHeader because the two have different scopes and
// different lifetimes, and conflating them cost real availability. The archive
// ID is minted once for the archive; it is not produced by writing a volume.
// While it lived only in the volume header, a first write that was interrupted
// after stamping the medium and before the catalog was saved lost the ID
// entirely — the next run minted a different one and refused the cartridge it
// had itself stamped, permanently. With the identity in an object of its own,
// any volume can hand it back, so the local catalog can be repaired from the
// medium instead of the medium being orphaned by the catalog.
//
// The split also settles the question §3.1 left open: the KDF salt is per
// archive, not per volume. It has to be, or an object migrated between volumes
// could not be decrypted from its own record — which is exactly what §9.1
// requires of ciphertext migration. Being byte-identical on every volume is
// therefore a property to preserve, not an accident.
type ArchiveHeader struct {
	Magic   string `json:"magic"`
	Format  int    `json:"format"`
	Archive string `json:"archive"`

	// Encrypted and Cipher describe how this archive's objects were written.
	// Reading dispatches on what the header declares; writing always uses the
	// current default. That is what lets the format outlive its own algorithms,
	// which matters more here than in most formats because this medium cannot
	// be re-encrypted in place.
	Encrypted bool           `json:"encrypted"`
	Cipher    string         `json:"cipher,omitempty"`
	KDF       string         `json:"kdf,omitempty"`
	KDFParams map[string]any `json:"kdf_params,omitempty"`
}

// VolumeHeader is what is true of this cartridge alone (DESIGN §3.1): which
// archive it belongs to, and which cartridge of it this is.
//
// Two fields, and no more, because everything here is written once and never
// rewritten. A generation number and a "written" date used to live here and had
// to leave: on this medium an update is not an update. LTFS appends the new
// bytes and abandons the old blocks, so a header with a mutable field costs a
// fresh copy of itself every cycle and strands the last one. Anything that
// changes is versioned under generations/ instead.
//
// Losing the date costs nothing that mattered. It was there so twenty encrypted
// cartridges would not be twenty identical bricks; the generation directories
// answer that better, because "how far has this cartridge been written?" is
// readable from a listing without unlocking anything.
type VolumeHeader struct {
	Magic  string `json:"magic"`
	Format int    `json:"format"`

	// Archive repeats the ID from the archive header. Redundant on purpose: it
	// is what lets the two objects be cross-checked, so a volume assembled from
	// pieces of two archives is caught rather than half-read.
	Archive string `json:"archive"`
	Volume  string `json:"volume"`
}

// NewArchiveID mints an identifier for a new archive.
//
// Random rather than derived from a hostname or path: an archive outlives the
// machine that created it, and two archives over the same source — the second
// copy that §8.1 recommends instead of erasure coding — must not collide.
func NewArchiveID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// readJSON fetches one plaintext metadata object, or ErrBlank when it is absent.
func readJSON(ctx context.Context, vol volume.Volume, name string, v any) error {
	rc, err := vol.Get(ctx, name)
	if errors.Is(err, volume.ErrNotFound) {
		return ErrBlank
	}
	if err != nil {
		return err
	}
	defer rc.Close()

	b, err := io.ReadAll(io.LimitReader(rc, 1<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s on %s: %w", name, vol.ID(), err)
	}
	return nil
}

// ReadArchiveHeader returns what the volume says about the archive it belongs
// to, or ErrBlank when it carries no archive header.
func ReadArchiveHeader(ctx context.Context, vol volume.Volume) (*ArchiveHeader, error) {
	h := new(ArchiveHeader)
	if err := readJSON(ctx, vol, ArchiveObject, h); err != nil {
		return nil, err
	}
	if h.Magic != ArchiveMagic {
		return nil, fmt.Errorf("%s on %s: not a kura archive (magic %q)", ArchiveObject, vol.ID(), h.Magic)
	}
	if h.Format > HeaderFormat {
		return nil, fmt.Errorf("volume %s was written by a newer kura (archive format %d, this build understands %d)",
			vol.ID(), h.Format, HeaderFormat)
	}
	if h.Archive == "" {
		return nil, fmt.Errorf("%s on %s names no archive", ArchiveObject, vol.ID())
	}
	return h, nil
}

// ReadHeader returns the volume's own header, or ErrBlank when it has none.
func ReadHeader(ctx context.Context, vol volume.Volume) (*VolumeHeader, error) {
	h := new(VolumeHeader)
	if err := readJSON(ctx, vol, HeaderObject, h); err != nil {
		return nil, err
	}
	if h.Magic != HeaderMagic {
		return nil, fmt.Errorf("%s on %s: not a kura volume (magic %q)", HeaderObject, vol.ID(), h.Magic)
	}
	if h.Format > HeaderFormat {
		return nil, fmt.Errorf("volume %s was written by a newer kura (header format %d, this build understands %d)",
			vol.ID(), h.Format, HeaderFormat)
	}
	return h, nil
}

// ArchiveIDOn reports the archive a loaded volume belongs to, or "" when the
// volume is blank.
//
// This is what lets a lost archive ID be recovered rather than re-minted. A
// catalog with no archive ID and a cartridge that already declares one is not
// an ambiguous situation: the medium is the authority (DESIGN §11), so the
// catalog adopts what the medium says instead of minting a second identity for
// an archive that already has one.
func ArchiveIDOn(ctx context.Context, vol volume.Volume) (string, error) {
	h, err := ReadArchiveHeader(ctx, vol)
	if errors.Is(err, ErrBlank) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return h.Archive, nil
}

// ErrBlank means the volume carries no header: it has never been written to by
// this tool, or by anything.
var ErrBlank = errors.New("volume is blank")

// ErrForeign means the volume belongs to a different archive.
var ErrForeign = errors.New("volume belongs to another archive")

// Reconcile decides whether a volume may be written to as part of archiveID at
// this generation, and stamps a header on it if it is blank.
//
// The medium is the authority and the local catalog is a cache (DESIGN §11).
// This implements the rows that can be decided from the header alone; adopting
// a volume that is ahead, and merging a divergent one, need the volume's own
// catalog compared against the local one and are not built yet — both stop
// here rather than guessing, which is the point of the table.
func Reconcile(ctx context.Context, vol volume.Volume, archiveID string, gen int, dryRun bool) (*VolumeHeader, error) {
	if archiveID == "" {
		return nil, errors.New("archive: cannot reconcile without an archive ID")
	}

	// The archive header first, because it is the one that decides whether this
	// cartridge is ours at all. Checking identity before anything else is what
	// keeps a foreign volume from being reported as a volume-name mismatch.
	ah, err := ReadArchiveHeader(ctx, vol)
	switch {
	case errors.Is(err, ErrBlank):
		ah = &ArchiveHeader{Magic: ArchiveMagic, Format: HeaderFormat, Archive: archiveID}
		if !dryRun {
			if err := putJSON(ctx, vol, ArchiveObject, ah); err != nil {
				return nil, fmt.Errorf("stamping the archive header on %s: %w", vol.ID(), err)
			}
		}
	case err != nil:
		return nil, err
	case ah.Archive != archiveID:
		return nil, fmt.Errorf("%w: %s belongs to archive %s, this one is %s",
			ErrForeign, vol.ID(), ah.Archive, archiveID)
	case ah.Encrypted:
		return nil, fmt.Errorf("volume %s is encrypted and encryption is not implemented yet", vol.ID())
	}

	h, err := ReadHeader(ctx, vol)
	switch {
	case errors.Is(err, ErrBlank):
		// Reached either by a blank cartridge or by one whose archive header
		// was stamped and whose volume header was not — an interrupted first
		// write. Both are finished the same way, because the archive header
		// above has already established that the identity matches.
		h = &VolumeHeader{
			Magic:   HeaderMagic,
			Format:  HeaderFormat,
			Archive: archiveID,
			Volume:  vol.ID(),
		}
		if dryRun {
			return h, nil
		}
		if err := putJSON(ctx, vol, HeaderObject, h); err != nil {
			return nil, fmt.Errorf("stamping %s: %w", vol.ID(), err)
		}
		return h, nil

	case err != nil:
		return nil, err
	}

	if h.Archive != archiveID {
		// The two plaintext objects disagree, which the archive header check
		// above cannot catch on its own. A cartridge assembled from pieces of
		// two archives is a corrupt volume, not a foreign one.
		return nil, fmt.Errorf("volume %s: %s names archive %s but %s names %s; the volume's own metadata disagrees",
			vol.ID(), HeaderObject, h.Archive, ArchiveObject, archiveID)
	}
	if h.Volume != vol.ID() {
		// The medium says it is a different cartridge from the one the drive
		// reports. Never guess which is right.
		return nil, fmt.Errorf("volume %s carries a header for %s; refusing to write to a volume that cannot identify itself",
			vol.ID(), h.Volume)
	}

	// The "volume ahead" row of DESIGN §11, answered from what the cartridge
	// actually holds rather than from a date stamped when it entered service.
	// A volume written at generation 5 by another machine is ahead of a catalog
	// that only knows about 3, and adopting it needs the volume's own catalog
	// compared against the local one — which is not built yet, so this stops
	// rather than guessing.
	newest, err := NewestGeneration(ctx, vol)
	if err != nil {
		return nil, err
	}
	if newest > gen {
		return nil, fmt.Errorf("volume %s holds generation %d but this run is generation %d: the volume is ahead of the catalog, which needs an explicit adopt rather than a write",
			vol.ID(), newest, gen)
	}
	return h, nil
}

// NewestGeneration is the highest generation this volume carries metadata for,
// or 0 if it carries none.
//
// Answered from object names, so it costs a listing rather than a read.
func NewestGeneration(ctx context.Context, vol volume.Volume) (int, error) {
	objs, err := vol.List(ctx)
	if err != nil {
		return 0, err
	}
	newest := 0
	for _, o := range objs {
		if g := generationOf(o.Name); g > newest {
			newest = g
		}
	}
	return newest, nil
}

func putJSON(ctx context.Context, vol volume.Volume, name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return vol.Put(ctx, name, int64(len(b)), bytesReader(b))
}
