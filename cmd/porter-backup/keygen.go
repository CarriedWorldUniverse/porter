package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"

	casket "github.com/CarriedWorldUniverse/casket-go"
)

// runKeygen generates a fresh X25519 recipient keypair, writes the PRIVATE
// key (base64, one line) to outPath with 0600 perms, and prints the PUBLIC
// key (base64) to w — ready for PORTER_RECIPIENTS. The private file is the
// recovery secret: hand it to the operator for off-machine custody.
func runKeygen(outPath string, w io.Writer) error {
	priv, pub, err := casket.GenerateRecipientKey()
	if err != nil {
		return fmt.Errorf("generating recipient key: %w", err)
	}
	f, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating private key file: %w", err)
	}
	if _, err := fmt.Fprintln(f, base64.StdEncoding.EncodeToString(priv)); err != nil {
		f.Close()
		return fmt.Errorf("writing private key: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing private key: %w", err)
	}
	_, err = fmt.Fprintln(w, base64.StdEncoding.EncodeToString(pub))
	return err
}

// readPrivateKey loads a keygen-written private key file.
func readPrivateKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading private key: %w", err)
	}
	key, err := base64.StdEncoding.DecodeString(string(trimNL(data)))
	if err != nil {
		return nil, fmt.Errorf("private key file %s: not valid base64: %w", path, err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("private key file %s: want 32 bytes, got %d", path, len(key))
	}
	return key, nil
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}
