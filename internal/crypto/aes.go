package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// Encryptor provides AES-256-GCM envelope encryption.
// The nonce is prepended to the ciphertext output.
type Encryptor struct {
	key []byte // must be 32 bytes for AES-256
}

func NewEncryptor(key []byte) (*Encryptor, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("crypto: AES-256 key must be 32 bytes, got %d", len(key))
	}
	return &Encryptor{key: key}, nil
}

// Seal encrypts plaintext. Returns nonce||ciphertext.
func (e *Encryptor) Seal(plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(e.key)
	if err != nil {
		return nil, fmt.Errorf("crypto: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: new gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("crypto: generate nonce: %w", err)
	}
	// Seal appends ciphertext+tag to nonce slice — single allocation.
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Open decrypts nonce||ciphertext produced by Seal.
func (e *Encryptor) Open(ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(e.key)
	if err != nil {
		return nil, fmt.Errorf("crypto: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: new gcm: %w", err)
	}
	if len(ciphertext) < gcm.NonceSize() {
		return nil, errors.New("crypto: ciphertext too short")
	}
	nonce, ct := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("crypto: decrypt: %w", err)
	}
	return plain, nil
}
