package worker

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"testing"

	"golang.org/x/crypto/ssh"
)

func generateKey(t *testing.T, path string) (ssh.Signer, ssh.PublicKey) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer, signer.PublicKey()
}
