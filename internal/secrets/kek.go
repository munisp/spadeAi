// Package secrets seals small secrets (Nebula CA keys, backup repository passwords) with a platform
// key-encryption key. Locally the KEK is a 0600 file; production must supply it from a KMS/Vault mount.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type KEK struct{ aead cipher.AEAD }

// LoadOrCreate reads a 32-byte KEK from path, creating it (0600) if absent and create is true.
func LoadOrCreate(path string, create bool) (*KEK, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && create {
		b = make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, b, 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, fmt.Errorf("KEK unavailable: %w", err)
	}
	return New(b)
}

func New(key []byte) (*KEK, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("KEK must be 32 bytes, got %d", len(key))
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	return &KEK{aead: aead}, nil
}

// Seal encrypts plaintext bound to aad (e.g. "overlay_ca:<project>") so a sealed value cannot be replayed elsewhere.
func (k *KEK) Seal(plaintext []byte, aad string) ([]byte, error) {
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return k.aead.Seal(nonce, nonce, plaintext, []byte(aad)), nil
}

func (k *KEK) Open(sealed []byte, aad string) ([]byte, error) {
	n := k.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("sealed value too short")
	}
	return k.aead.Open(nil, sealed[:n], sealed[n:], []byte(aad))
}
