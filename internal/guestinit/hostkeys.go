package guestinit

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
)

const hostKeyDir = "var/lib/observatory/ssh"

// hostKeyFiles has no RSA key: our SSH server does not need one, and generating it costs seconds on a small VM.
var hostKeyFiles = []string{"ssh_host_ed25519_key", "ssh_host_ecdsa_key"}

// HostKeys loads the guest's SSH host keys, creating missing ones; regenerate replaces all of them, as a takeoff must.
func (r Root) HostKeys(regenerate bool) ([]ssh.Signer, error) {
	dir := r.join(hostKeyDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	var signers []ssh.Signer
	for _, name := range hostKeyFiles {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if regenerate || errors.Is(err, fs.ErrNotExist) {
			data, err = newHostKey(name)
			if err == nil {
				err = writeFileAtomic(path, data, 0o600)
			}
		}
		if err != nil {
			return nil, err
		}
		s, err := ssh.ParsePrivateKey(data)
		if err != nil {
			return nil, err
		}
		signers = append(signers, s)
	}
	return signers, nil
}

func newHostKey(name string) ([]byte, error) {
	var key crypto.PrivateKey
	var err error
	if name == "ssh_host_ed25519_key" {
		_, key, err = ed25519.GenerateKey(rand.Reader)
	} else {
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(block), nil
}
