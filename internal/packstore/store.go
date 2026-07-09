package packstore

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/CarriedWorldUniverse/porter/internal/envelope"
)

// Reader is a read-only, point-in-time view of a packstore: the latest
// superblock (as of open) and its index. It needs only ONE recipient
// private key — this is the recovery path.
type Reader struct {
	b        Backend
	privKey  []byte
	storeID  string
	idx      index
	packSize int

	packCache map[string][]byte
}

// Writer is a Reader plus the staging area and recipient set needed to
// append new generations to the store.
type Writer struct {
	*Reader

	recipients [][]byte
	chunkSize  int
	gen        uint64
	staged     map[string][]byte
}

// Writer embeds *Reader, so List and Get are available directly on a
// Writer (w.List(), w.Get(name)) without an accessor.

// Init creates a brand-new packstore in b: an empty (gen 0) index and its
// superblock. packSize and chunkSize are fixed for the store's lifetime;
// chunkSize must not exceed packSize.
func Init(b Backend, recipients [][]byte, packSize, chunkSize int) (*Writer, error) {
	if len(recipients) == 0 {
		return nil, fmt.Errorf("packstore: init: no recipients")
	}
	if packSize <= 0 || chunkSize <= 0 {
		return nil, fmt.Errorf("packstore: init: pack_size and chunk_size must be positive")
	}
	if chunkSize > packSize {
		return nil, fmt.Errorf("packstore: init: chunk_size (%d) must not exceed pack_size (%d)", chunkSize, packSize)
	}

	storeID, err := randomHex(16)
	if err != nil {
		return nil, fmt.Errorf("packstore: init: %w", err)
	}

	idx := newIndex()
	idxName, err := writeIndex(b, recipients, storeID, 0, idx)
	if err != nil {
		return nil, fmt.Errorf("packstore: init: %w", err)
	}
	sb := superblock{
		FormatVersion: formatVersion,
		StoreID:       storeID,
		PackSize:      packSize,
		ChunkSize:     chunkSize,
		Gen:           0,
		IndexObject:   idxName,
	}
	if _, err := writeSuperblock(b, recipients, sb); err != nil {
		return nil, fmt.Errorf("packstore: init: %w", err)
	}

	return &Writer{
		Reader: &Reader{
			b:         b,
			storeID:   storeID,
			idx:       idx,
			packSize:  packSize,
			packCache: map[string][]byte{},
		},
		recipients: recipients,
		chunkSize:  chunkSize,
		gen:        0,
		staged:     map[string][]byte{},
	}, nil
}

// OpenWriter loads the latest generation of a packstore from b and returns
// a Writer able to append new generations to it.
func OpenWriter(b Backend, privKey []byte, recipients [][]byte) (*Writer, error) {
	if len(recipients) == 0 {
		return nil, fmt.Errorf("packstore: open writer: no recipients")
	}
	sb, err := readLatestSuperblock(b, privKey)
	if err != nil {
		return nil, fmt.Errorf("packstore: open writer: %w", err)
	}
	idx, err := readIndex(b, privKey, sb.StoreID, sb.IndexObject)
	if err != nil {
		return nil, fmt.Errorf("packstore: open writer: %w", err)
	}
	return &Writer{
		Reader: &Reader{
			b:         b,
			privKey:   privKey,
			storeID:   sb.StoreID,
			idx:       idx,
			packSize:  sb.PackSize,
			packCache: map[string][]byte{},
		},
		recipients: recipients,
		chunkSize:  sb.ChunkSize,
		gen:        sb.Gen,
		staged:     map[string][]byte{},
	}, nil
}

// OpenReader loads the latest generation of a packstore from b using a
// single recipient private key — the recovery path. It does not need the
// full recipient set.
func OpenReader(b Backend, privKey []byte) (*Reader, error) {
	sb, err := readLatestSuperblock(b, privKey)
	if err != nil {
		return nil, fmt.Errorf("packstore: open reader: %w", err)
	}
	idx, err := readIndex(b, privKey, sb.StoreID, sb.IndexObject)
	if err != nil {
		return nil, fmt.Errorf("packstore: open reader: %w", err)
	}
	return &Reader{
		b:         b,
		privKey:   privKey,
		storeID:   sb.StoreID,
		idx:       idx,
		packSize:  sb.PackSize,
		packCache: map[string][]byte{},
	}, nil
}

// Put stages an artifact for the next Commit. It does not touch the store
// until Commit runs; a repeated Put under the same name before Commit
// simply replaces the staged data.
func (w *Writer) Put(name string, data []byte) {
	cp := make([]byte, len(data))
	copy(cp, data)
	w.staged[name] = cp
}

// Commit runs one epoch: it chunks every staged artifact, dedups
// content-addressed chunks against the existing index and against each
// other, packs the new chunks into pack_size objects (padding the last
// with random bytes), uploads packs, then a new index generation, then a
// new superblock generation — in that order, so a crash mid-commit leaves
// only harmless orphans. Staging is cleared on success (and on failure,
// since a failed commit's staged data may be partially durable already).
func (w *Writer) Commit() error {
	if len(w.staged) == 0 {
		return nil
	}
	defer func() { w.staged = map[string][]byte{} }()

	newChunkIDs, newChunkData, newNames := w.chunkStaged()

	newLocs, err := w.packAndUpload(newChunkIDs, newChunkData)
	if err != nil {
		return fmt.Errorf("packstore: commit: %w", err)
	}

	nextIdx := index{
		Chunks: make(map[string]chunkLoc, len(w.idx.Chunks)+len(newLocs)),
		Names:  make(map[string]nameEntry, len(w.idx.Names)+len(newNames)),
	}
	for id, loc := range w.idx.Chunks {
		nextIdx.Chunks[id] = loc
	}
	for id, loc := range newLocs {
		nextIdx.Chunks[id] = loc
	}
	for name, entry := range w.idx.Names {
		nextIdx.Names[name] = entry
	}
	for name, entry := range newNames {
		nextIdx.Names[name] = entry
	}

	gen := w.gen + 1
	idxName, err := writeIndex(w.b, w.recipients, w.storeID, gen, nextIdx)
	if err != nil {
		return fmt.Errorf("packstore: commit: %w", err)
	}
	sb := superblock{
		FormatVersion: formatVersion,
		StoreID:       w.storeID,
		PackSize:      w.packSize,
		ChunkSize:     w.chunkSize,
		Gen:           gen,
		IndexObject:   idxName,
	}
	if _, err := writeSuperblock(w.b, w.recipients, sb); err != nil {
		return fmt.Errorf("packstore: commit: %w", err)
	}

	w.gen = gen
	w.idx = nextIdx
	return nil
}

// chunkStaged splits every staged artifact into chunkSize pieces, computes
// each chunk's content id (sha256 hex), and returns the set of chunk ids
// that are genuinely new (not already in the index and not a duplicate
// within this batch) in first-seen order, plus each artifact's nameEntry.
func (w *Writer) chunkStaged() (newChunkIDs []string, newChunkData map[string][]byte, newNames map[string]nameEntry) {
	newChunkData = map[string][]byte{}
	newNames = map[string]nameEntry{}
	seen := map[string]bool{}

	// Deterministic artifact order keeps Commit's output reproducible,
	// which makes tests and debugging saner.
	names := make([]string, 0, len(w.staged))
	for name := range w.staged {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		data := w.staged[name]
		var chunkIDs []string
		for off := 0; off < len(data); off += w.chunkSize {
			end := off + w.chunkSize
			if end > len(data) {
				end = len(data)
			}
			chunk := data[off:end]
			sum := sha256.Sum256(chunk)
			id := hex.EncodeToString(sum[:])
			chunkIDs = append(chunkIDs, id)

			if _, exists := w.idx.Chunks[id]; exists {
				continue
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			newChunkIDs = append(newChunkIDs, id)
			newChunkData[id] = chunk
		}
		newNames[name] = nameEntry{Chunks: chunkIDs, Size: int64(len(data))}
	}
	return newChunkIDs, newChunkData, newNames
}

// packAndUpload fills pack_size-byte packs with the given new chunks (in
// chunkIDs order), pads the last pack with random bytes, seals and uploads
// each pack, and returns the resulting chunk locations.
func (w *Writer) packAndUpload(chunkIDs []string, data map[string][]byte) (map[string]chunkLoc, error) {
	locs := make(map[string]chunkLoc, len(chunkIDs))
	if len(chunkIDs) == 0 {
		return locs, nil
	}

	buf := make([]byte, 0, w.packSize)
	type pending struct {
		id     string
		offset int
	}
	var inPack []pending

	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		padded := make([]byte, w.packSize)
		copy(padded, buf)
		if _, err := rand.Read(padded[len(buf):]); err != nil {
			return fmt.Errorf("padding pack: %w", err)
		}
		name, err := packName()
		if err != nil {
			return fmt.Errorf("naming pack: %w", err)
		}
		sealed, err := envelope.Seal(padded, w.recipients, objectPath(w.storeID, name))
		if err != nil {
			return fmt.Errorf("sealing pack: %w", err)
		}
		if err := w.b.Put(name, sealed); err != nil {
			return fmt.Errorf("uploading pack: %w", err)
		}
		for _, p := range inPack {
			locs[p.id] = chunkLoc{Pack: name, Offset: p.offset, Length: len(data[p.id])}
		}
		buf = buf[:0]
		inPack = inPack[:0]
		return nil
	}

	for _, id := range chunkIDs {
		chunk := data[id]
		if len(chunk) > w.packSize {
			return nil, fmt.Errorf("chunk %s (%d bytes) exceeds pack_size (%d)", id, len(chunk), w.packSize)
		}
		if len(buf)+len(chunk) > w.packSize {
			if err := flush(); err != nil {
				return nil, err
			}
		}
		inPack = append(inPack, pending{id: id, offset: len(buf)})
		buf = append(buf, chunk...)
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return locs, nil
}
