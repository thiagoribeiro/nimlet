package domain

import (
	"fmt"
	"unicode"
	"unicode/utf8"
)

// Field limits. They bound memory and storage per request and keep identifiers usable;
// every entry point (HTTP, AppService, CLI) enforces the same ones.
const (
	MaxUsernameLen    = 64   // runes
	MaxDisplayNameLen = 256  // runes
	MaxDeviceNameLen  = 256  // runes
	MaxRoomNameLen    = 256  // runes
	MaxAliasLen       = 255  // runes
	MaxEventTypeLen   = 255  // bytes
	MaxTxnIDLen       = 255  // bytes
	MaxPasswordBytes  = 1024 // Argon2 hashes the whole input: bound the work per request
)

func invalidField(field, format string, args ...any) error {
	return fmt.Errorf("%s %s: %w", field, fmt.Sprintf(format, args...), ErrInvalidArgument)
}

// ValidateUsername: 1–64 characters, no whitespace or control characters.
func ValidateUsername(u string) error {
	n := utf8.RuneCountInString(u)
	if n == 0 || n > MaxUsernameLen {
		return invalidField("username", "must be 1-%d characters", MaxUsernameLen)
	}
	for _, r := range u {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return invalidField("username", "must not contain spaces or control characters")
		}
	}
	return nil
}

// ValidatePassword: non-empty, at most MaxPasswordBytes.
func ValidatePassword(p string) error {
	if p == "" || len(p) > MaxPasswordBytes {
		return invalidField("password", "must be 1-%d bytes", MaxPasswordBytes)
	}
	return nil
}

// ValidateText checks an optional free-text field: at most max characters, valid UTF-8.
func ValidateText(field, v string, max int) error {
	if !utf8.ValidString(v) || utf8.RuneCountInString(v) > max {
		return invalidField(field, "must be valid text of at most %d characters", max)
	}
	return nil
}

// ValidateToken checks a required opaque identifier from a URL (event type, txn ID).
func ValidateToken(field, v string, max int) error {
	if v == "" || len(v) > max {
		return invalidField(field, "must be 1-%d bytes", max)
	}
	return nil
}
