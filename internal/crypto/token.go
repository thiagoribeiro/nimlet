package crypto

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
)

const tokenIDLen = 32

// NewSessionToken generates a new opaque session token.
// Format: base64url(sessionID) + "." + base64url(HMAC-SHA256(sessionID, secret))
// The sessionID is embedded so auth middleware can look it up in DB without full table scan.
func NewSessionToken(sessionID string, secret []byte) (token string, err error) {
	idPart := base64.RawURLEncoding.EncodeToString([]byte(sessionID))
	sig := computeHMAC([]byte(sessionID), secret)
	sigPart := base64.RawURLEncoding.EncodeToString(sig)
	return idPart + "." + sigPart, nil
}

// ParseSessionToken extracts sessionID from the token and verifies the HMAC.
func ParseSessionToken(token string, secret []byte) (sessionID string, err error) {
	dot := strings.LastIndexByte(token, '.')
	if dot < 0 {
		return "", fmt.Errorf("crypto: malformed token")
	}
	idBytes, err := base64.RawURLEncoding.DecodeString(token[:dot])
	if err != nil {
		return "", fmt.Errorf("crypto: decode token id: %w", err)
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(token[dot+1:])
	if err != nil {
		return "", fmt.Errorf("crypto: decode token sig: %w", err)
	}
	expected := computeHMAC(idBytes, secret)
	if !hmac.Equal(sigBytes, expected) {
		return "", fmt.Errorf("crypto: invalid token signature")
	}
	return string(idBytes), nil
}

// TokenHash returns a SHA-256 hash of the token for DB storage.
// Stored in sessions.token_hash for fast revocation lookup.
func TokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return base64.RawStdEncoding.EncodeToString(h[:])
}

// GenerateID generates a cryptographically random 32-byte ID encoded as base64url.
func GenerateID() (string, error) {
	b := make([]byte, tokenIDLen)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto: generate id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func computeHMAC(data, secret []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write(data)
	return mac.Sum(nil)
}
