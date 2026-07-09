package packstore

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/CarriedWorldUniverse/porter/internal/envelope"
)

const formatVersion = 1

// superblockPathPrefix is the fixed objectPath segment used for sealing
// superblocks. A superblock must be openable before the store_id it names
// is known (that's the whole point of a superblock), so it cannot be
// authenticated under a path that embeds store_id like every other object
// is. All superblocks across all packstore stores therefore share this one
// AAD path segment.
const superblockPathPrefix = "packstore/sb/"

// superblock is the write-last, generation-versioned root of a packstore.
// The latest superblock (highest gen, i.e. lexicographically greatest
// object name under "sb-") is authoritative.
type superblock struct {
	FormatVersion int    `json:"format_version"`
	StoreID       string `json:"store_id"`
	PackSize      int    `json:"pack_size"`
	ChunkSize     int    `json:"chunk_size"`
	Gen           uint64 `json:"gen"`
	IndexObject   string `json:"index_object"`
}

// chunkLoc locates one content-addressed chunk within a pack's UNSEALED
// (plaintext) byte stream.
type chunkLoc struct {
	Pack   string `json:"pack"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}

// nameEntry is one artifact's chunk list, in order, plus its total
// plaintext size.
type nameEntry struct {
	Chunks []string `json:"chunks"`
	Size   int64    `json:"size"`
}

// index is a full snapshot of the store's chunk and artifact-name mappings
// as of one generation. A fresh index is written in full every Commit.
type index struct {
	Chunks map[string]chunkLoc  `json:"chunks"`
	Names  map[string]nameEntry `json:"names"`
}

func newIndex() index {
	return index{Chunks: map[string]chunkLoc{}, Names: map[string]nameEntry{}}
}

// randomHex returns n random bytes hex-encoded.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("packstore: reading random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// objectPath returns the AAD object path for a non-superblock object
// (index or pack) belonging to storeID.
func objectPath(storeID, name string) string {
	return "packstore/" + storeID + "/" + name
}

// superblockObjectPath returns the AAD object path for a superblock object.
func superblockObjectPath(name string) string {
	return superblockPathPrefix + name
}

// superblockName builds a superblock object name for a generation.
func superblockName(gen uint64) (string, error) {
	suffix, err := randomHex(4)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sb-%016d-%s", gen, suffix), nil
}

// indexName builds an index object name for a generation.
func indexName(gen uint64) (string, error) {
	suffix, err := randomHex(4)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("idx-%016d-%s", gen, suffix), nil
}

// packName builds a pack object name.
func packName() (string, error) {
	suffix, err := randomHex(16)
	if err != nil {
		return "", err
	}
	return "pack-" + suffix, nil
}

// writeSuperblock seals and Puts a superblock.
func writeSuperblock(b Backend, recipients [][]byte, sb superblock) (string, error) {
	name, err := superblockName(sb.Gen)
	if err != nil {
		return "", fmt.Errorf("packstore: naming superblock: %w", err)
	}
	plain, err := json.Marshal(sb)
	if err != nil {
		return "", fmt.Errorf("packstore: marshalling superblock: %w", err)
	}
	sealed, err := envelope.Seal(plain, recipients, superblockObjectPath(name))
	if err != nil {
		return "", fmt.Errorf("packstore: sealing superblock: %w", err)
	}
	if err := b.Put(name, sealed); err != nil {
		return "", fmt.Errorf("packstore: writing superblock: %w", err)
	}
	return name, nil
}

// readLatestSuperblock finds and unseals the highest-generation superblock
// in b.
func readLatestSuperblock(b Backend, privKey []byte) (superblock, error) {
	names, err := b.List("sb-")
	if err != nil {
		return superblock{}, fmt.Errorf("packstore: listing superblocks: %w", err)
	}
	if len(names) == 0 {
		return superblock{}, fmt.Errorf("packstore: no superblock found (store not initialized?)")
	}
	name := names[len(names)-1]
	sealed, err := b.Get(name)
	if err != nil {
		return superblock{}, fmt.Errorf("packstore: reading superblock %s: %w", name, err)
	}
	plain, err := envelope.Unseal(privKey, sealed, superblockObjectPath(name))
	if err != nil {
		return superblock{}, fmt.Errorf("packstore: unsealing superblock %s: %w", name, err)
	}
	var sb superblock
	if err := json.Unmarshal(plain, &sb); err != nil {
		return superblock{}, fmt.Errorf("packstore: decoding superblock %s: %w", name, err)
	}
	return sb, nil
}

// writeIndex seals and Puts an index for storeID at gen.
func writeIndex(b Backend, recipients [][]byte, storeID string, gen uint64, idx index) (string, error) {
	name, err := indexName(gen)
	if err != nil {
		return "", fmt.Errorf("packstore: naming index: %w", err)
	}
	plain, err := json.Marshal(idx)
	if err != nil {
		return "", fmt.Errorf("packstore: marshalling index: %w", err)
	}
	sealed, err := envelope.Seal(plain, recipients, objectPath(storeID, name))
	if err != nil {
		return "", fmt.Errorf("packstore: sealing index: %w", err)
	}
	if err := b.Put(name, sealed); err != nil {
		return "", fmt.Errorf("packstore: writing index: %w", err)
	}
	return name, nil
}

// readIndex reads and unseals the index object named indexObject for
// storeID.
func readIndex(b Backend, privKey []byte, storeID, indexObject string) (index, error) {
	sealed, err := b.Get(indexObject)
	if err != nil {
		return index{}, fmt.Errorf("packstore: reading index %s: %w", indexObject, err)
	}
	plain, err := envelope.Unseal(privKey, sealed, objectPath(storeID, indexObject))
	if err != nil {
		return index{}, fmt.Errorf("packstore: unsealing index %s: %w", indexObject, err)
	}
	var idx index
	if err := json.Unmarshal(plain, &idx); err != nil {
		return index{}, fmt.Errorf("packstore: decoding index %s: %w", indexObject, err)
	}
	if idx.Chunks == nil {
		idx.Chunks = map[string]chunkLoc{}
	}
	if idx.Names == nil {
		idx.Names = map[string]nameEntry{}
	}
	return idx, nil
}
