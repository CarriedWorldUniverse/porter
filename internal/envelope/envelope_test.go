package envelope

import (
	"bytes"
	"errors"
	"testing"

	casket "github.com/CarriedWorldUniverse/casket-go"
)

func keypair(t *testing.T) (priv, pub []byte) {
	t.Helper()
	priv, pub, err := casket.GenerateRecipientKey()
	if err != nil {
		t.Fatalf("GenerateRecipientKey: %v", err)
	}
	return priv, pub
}

func TestSealUnsealRoundTripEitherKey(t *testing.T) {
	clusterPriv, clusterPub := keypair(t)
	recoveryPriv, recoveryPub := keypair(t)
	plaintext := []byte("snapshot bytes, definitely a sqlite db")
	objectPath := "backups/almanac/20260612T120000Z.casket"

	blob, err := Seal(plaintext, [][]byte{clusterPub, recoveryPub}, objectPath)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(blob, plaintext) {
		t.Fatal("sealed blob contains plaintext")
	}

	for name, priv := range map[string][]byte{"cluster": clusterPriv, "recovery": recoveryPriv} {
		got, err := Unseal(priv, blob, objectPath)
		if err != nil {
			t.Fatalf("Unseal(%s): %v", name, err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Fatalf("Unseal(%s): plaintext mismatch", name)
		}
	}
}

func TestUnsealWrongObjectPathFails(t *testing.T) {
	// The object path is AAD — porter's path convention is part of what is
	// authenticated. A blob renamed/moved on Drive must not open under the
	// wrong path.
	priv, pub := keypair(t)
	blob, err := Seal([]byte("x"), [][]byte{pub}, "backups/almanac/a.casket")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := Unseal(priv, blob, "backups/herald/a.casket"); !errors.Is(err, casket.ErrEnvelopeOpen) {
		t.Fatalf("Unseal(wrong path): got %v, want ErrEnvelopeOpen", err)
	}
}

func TestUnsealTamperedBlobFails(t *testing.T) {
	priv, pub := keypair(t)
	blob, err := Seal([]byte("payload to protect"), [][]byte{pub}, "p")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// Flip one bit in the BODY (past header + the single wrap entry) so the
	// AEAD open fails loudly.
	tampered := append([]byte(nil), blob...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := Unseal(priv, tampered, "p"); !errors.Is(err, casket.ErrEnvelopeOpen) {
		t.Fatalf("Unseal(tampered): got %v, want ErrEnvelopeOpen", err)
	}
}

func TestUnsealWrongKeyFails(t *testing.T) {
	_, pub := keypair(t)
	otherPriv, _ := keypair(t)
	blob, err := Seal([]byte("x"), [][]byte{pub}, "p")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	_, err = Unseal(otherPriv, blob, "p")
	if !errors.Is(err, casket.ErrNoRecipient) {
		t.Fatalf("Unseal(wrong key): got %v, want ErrNoRecipient", err)
	}
}

func TestRecipientIDsHex(t *testing.T) {
	_, pub1 := keypair(t)
	_, pub2 := keypair(t)
	blob, err := Seal([]byte("x"), [][]byte{pub1, pub2}, "p")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	ids, err := RecipientIDsHex(blob)
	if err != nil {
		t.Fatalf("RecipientIDsHex: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("got %d ids, want 2", len(ids))
	}
	for _, id := range ids {
		if len(id) != 16 { // 8 bytes hex
			t.Fatalf("id %q: want 16 hex chars", id)
		}
	}
	if ids[0] == ids[1] {
		t.Fatal("distinct recipients produced identical key ids")
	}
}

func TestSealNoRecipients(t *testing.T) {
	if _, err := Seal([]byte("x"), nil, "p"); err == nil {
		t.Fatal("Seal(no recipients): want error")
	}
}
