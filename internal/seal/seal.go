package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
)

type Key [32]byte

func ParseKey(b64 string) (Key, error) {
	var k Key
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return k, errors.New("not valid base64")
	}
	if len(raw) != len(k) {
		return k, errors.New("must decode to 32 bytes")
	}
	copy(k[:], raw)
	return k, nil
}

// ID identifies the key without revealing it.
func (k Key) ID() string {
	sum := sha256.Sum256(k[:])
	return hex.EncodeToString(sum[:8])
}

func gcm(k Key) (cipher.AEAD, error) {
	block, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func Seal(k Key, plaintext, aad []byte) (nonce, ciphertext []byte, err error) {
	g, err := gcm(k)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return nonce, g.Seal(nil, nonce, plaintext, aad), nil
}

func Open(k Key, nonce, ciphertext, aad []byte) ([]byte, error) {
	g, err := gcm(k)
	if err != nil {
		return nil, err
	}
	if len(nonce) != g.NonceSize() {
		return nil, errors.New("bad nonce length")
	}
	return g.Open(nil, nonce, ciphertext, aad)
}
