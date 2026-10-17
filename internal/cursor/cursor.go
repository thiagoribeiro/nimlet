// Package cursor turns internal stream positions into opaque tokens for clients.
//
// stream_pos comes from one sequence shared by every namespace, so exposing it lets a
// client measure other tenants' traffic from the gaps between its own events. Clients get
// AES-256-GCM-sealed tokens instead: unreadable, tamper-evident, and bound to the user they
// were issued to (the user ID is authenticated as additional data, so a token decrypts only
// for that user). Internally everything stays ordered by stream_pos.
package cursor

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
)

// Kind separates cursors that must not be swapped for one another. Only /sync uses one
// today (since / next_batch): room history pages by room_seq, which is safe to expose.
type Kind byte

const Sync Kind = 's'

// version prefixes every token; bump it (and keep accepting the old one for a while) when
// the key or layout changes.
const version = "c1."

// ErrInvalid covers every token that cannot be accepted: malformed, tampered with, issued to
// another user, of another kind, or from an unknown version. Callers answer 400 and the
// client starts over without a cursor.
var ErrInvalid = errors.New("cursor: invalid")

// Codec seals and opens cursors. Safe for concurrent use.
type Codec struct {
	aead cipher.AEAD
}

// New derives the cursor key from the server secret (HKDF-SHA256, a dedicated info label so
// it never equals a key used for anything else).
func New(secret []byte) (*Codec, error) {
	key, err := hkdf.Key(sha256.New, secret, nil, "nimlet-cursor-v1", 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Codec{aead: aead}, nil
}

func additionalData(kind Kind, userID string) []byte {
	return append([]byte{version[1], byte(kind)}, userID...)
}

// Encode seals pos for userID.
func (c *Codec) Encode(kind Kind, userID string, pos int64) string {
	nonce := make([]byte, c.aead.NonceSize(), c.aead.NonceSize()+8+c.aead.Overhead())
	_, _ = rand.Read(nonce) // crypto/rand never fails on supported platforms
	var plain [8]byte
	binary.BigEndian.PutUint64(plain[:], uint64(pos))
	sealed := c.aead.Seal(nonce, nonce, plain[:], additionalData(kind, userID))
	return version + base64.RawURLEncoding.EncodeToString(sealed)
}

// Decode opens a token issued to userID. An empty token means "from the start" (0).
func (c *Codec) Decode(kind Kind, userID, token string) (int64, error) {
	if token == "" {
		return 0, nil
	}
	body, ok := strings.CutPrefix(token, version)
	if !ok {
		return 0, ErrInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || len(raw) < c.aead.NonceSize() {
		return 0, ErrInvalid
	}
	nonce, sealed := raw[:c.aead.NonceSize()], raw[c.aead.NonceSize():]
	plain, err := c.aead.Open(nil, nonce, sealed, additionalData(kind, userID))
	if err != nil || len(plain) != 8 {
		return 0, ErrInvalid
	}
	return int64(binary.BigEndian.Uint64(plain)), nil
}
