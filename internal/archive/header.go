package archive

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"kura/internal/volume"
)

// HeaderMagic marks an object as a kura volume header.
const HeaderMagic = "kura-volume"

// HeaderFormat versions the header itself. It is deliberately separate from the
// catalog and plan formats: this is the one structure a future reader must be
// able to parse before it knows anything else about the volume.
const HeaderFormat = 1

// VolumeHeader is the single plaintext object on every volume (DESIGN §3.1).
//
// Generation and Written are a judgement call: strictly they leak an activity
// timeline. Without them twenty encrypted cartridges are twenty identical
// bricks and you cannot tell which is newest without unlocking each, which was
// the entire reason for having a header.
type VolumeHeader struct {
	Magic   string `json:"magic"`
	Format  int    `json:"format"`
	Archive string `json:"archive"`
	Volume  string `json:"volume"`

	// Encrypted and Cipher describe how the objects on this volume were
	// written. Reading dispatches on what the header declares; writing always
	// uses the current default. That is what lets the format outlive its own
	// algorithms, which matters more here than in most formats because this
	// medium cannot be re-encrypted in place.
	Encrypted bool   `json:"encrypted"`
	Cipher    string `json:"cipher,omitempty"`
	KDF       string `json:"kdf,omitempty"`

	// Generation and Written are when this volume was first written, and are
	// never updated: object names are write-once, and the header has no claim
	// to be the exception. A volume appended to over several cycles therefore
	// says when it entered service, and the catalog it carries says the rest.
	Generation int    `json:"generation"`
	Written    string `json:"written,omitempty"`
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

// ReadHeader returns the volume's header, or ErrBlank when it has none.
func ReadHeader(ctx context.Context, vol volume.Volume) (*VolumeHeader, error) {
	rc, err := vol.Get(ctx, HeaderObject)
	if errors.Is(err, volume.ErrNotFound) {
		return nil, ErrBlank
	}
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	b, err := io.ReadAll(io.LimitReader(rc, 1<<20))
	if err != nil {
		return nil, err
	}
	h := new(VolumeHeader)
	if err := json.Unmarshal(b, h); err != nil {
		return nil, fmt.Errorf("%s on %s: %w", HeaderObject, vol.ID(), err)
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
	h, err := ReadHeader(ctx, vol)
	switch {
	case errors.Is(err, ErrBlank):
		h = &VolumeHeader{
			Magic:      HeaderMagic,
			Format:     HeaderFormat,
			Archive:    archiveID,
			Volume:     vol.ID(),
			Generation: gen,
			Written:    time.Now().UTC().Format(time.RFC3339),
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
		return nil, fmt.Errorf("%w: %s belongs to archive %s, this one is %s",
			ErrForeign, vol.ID(), h.Archive, archiveID)
	}
	if h.Volume != vol.ID() {
		// The medium says it is a different cartridge from the one the drive
		// reports. Never guess which is right.
		return nil, fmt.Errorf("volume %s carries a header for %s; refusing to write to a volume that cannot identify itself",
			vol.ID(), h.Volume)
	}
	if h.Generation > gen {
		return nil, fmt.Errorf("volume %s holds generation %d but this run is generation %d: the volume is ahead of the catalog, which needs an explicit adopt rather than a write",
			vol.ID(), h.Generation, gen)
	}
	if h.Encrypted {
		return nil, fmt.Errorf("volume %s is encrypted and encryption is not implemented yet", vol.ID())
	}
	return h, nil
}

func putJSON(ctx context.Context, vol volume.Volume, name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return vol.Put(ctx, name, int64(len(b)), bytesReader(b))
}
