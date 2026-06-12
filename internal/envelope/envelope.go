// Package envelope is porter-backup's thin binding around casket-go's
// multi-recipient envelope. It pins porter's AAD conventions in ONE place:
// RepoIdentity is always "porter-backup", and ObjectPath is the
// Drive-relative object path (manifest.SnapshotObjectPath /
// ManifestObjectPath). Everything cryptographic is casket's; this package
// only guarantees that seal and restore agree on what is authenticated.
package envelope

import (
	"encoding/hex"

	casket "github.com/CarriedWorldUniverse/casket-go"
)

// RepoIdentity is the casket AAD repo identity for every porter-backup blob.
const RepoIdentity = "porter-backup"

// Seal encrypts an artifact for a set of X25519 recipient public keys (raw 32
// bytes each — casket.GenerateRecipientKey's pub). Any single corresponding
// private key can Unseal. objectPath is authenticated (AAD) and must be the
// blob's object path per the manifest package's conventions.
func Seal(plaintext []byte, recipients [][]byte, objectPath string) ([]byte, error) {
	return casket.SealMulti(plaintext, recipients, casket.SealOptions{
		RepoIdentity: []byte(RepoIdentity),
		ObjectPath:   []byte(objectPath),
	})
}

// Unseal decrypts a porter-backup blob with ONE recipient private key (raw 32
// bytes). objectPath must match what the blob was sealed under, or the open
// fails (it is AAD).
func Unseal(privKey, blob []byte, objectPath string) ([]byte, error) {
	return casket.OpenMulti(privKey, blob, []byte(RepoIdentity), []byte(objectPath))
}

// RecipientIDsHex lists a blob's recipient key ids as lowercase hex (8 bytes
// → 16 chars each), in wrap-entry order — the manifest's casket_keyids.
func RecipientIDsHex(blob []byte) ([]string, error) {
	ids, err := casket.RecipientIDs(blob)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = hex.EncodeToString(id)
	}
	return out, nil
}
