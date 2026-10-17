package crypto

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

const (
	argonTime    = 1
	argonMemory  = 64 * 1024
	argonThreads = 4
	argonKeyLen  = 32
	saltLen      = 16
)

// Each Argon2 run allocates argonMemory KiB (64 MiB). Without a cap, N concurrent logins
// need N×64 MiB — a few hundred requests exhaust the host. hashSlots bounds concurrent runs;
// callers wait up to hashQueueTimeout for a slot, then fail with ErrBusy (→ HTTP 503).
var (
	ErrBusy          = errors.New("crypto: password hashing at capacity")
	hashSlots        = make(chan struct{}, max(2, runtime.NumCPU()/2))
	hashQueueTimeout = 3 * time.Second
)

func withHashSlot(fn func()) error {
	timer := time.NewTimer(hashQueueTimeout)
	defer timer.Stop()
	select {
	case hashSlots <- struct{}{}:
		defer func() { <-hashSlots }()
		fn()
		return nil
	case <-timer.C:
		return ErrBusy
	}
}

// HashPassword hashes a plaintext password using Argon2id.
// Returns a PHC-formatted string: $argon2id$v=19$m=...,t=...,p=...$<salt>$<hash>
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("crypto: generate salt: %w", err)
	}
	var hash []byte
	if err := withHashSlot(func() {
		hash = argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	}); err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	), nil
}

// VerifyPassword checks a plaintext password against a PHC-formatted hash.
func VerifyPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 {
		return false, fmt.Errorf("crypto: invalid hash format")
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, fmt.Errorf("crypto: parse params: %w", err)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("crypto: decode salt: %w", err)
	}
	expectedHash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, fmt.Errorf("crypto: decode hash: %w", err)
	}
	var hash []byte
	if err := withHashSlot(func() {
		hash = argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(expectedHash)))
	}); err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(hash, expectedHash) == 1, nil
}
