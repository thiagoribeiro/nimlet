package config

import (
	"strings"
	"testing"
)

func TestLoad_RejectsInvalidIntegers(t *testing.T) {
	t.Setenv("NIMLET_DB_DSN", "postgres://x")
	t.Setenv("NIMLET_AUTH_HMAC_SECRET", strings.Repeat("s", 32))
	t.Setenv("NIMLET_DB_MAX_CONNS", "abc")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "NIMLET_DB_MAX_CONNS") {
		t.Fatalf("expected an error naming NIMLET_DB_MAX_CONNS, got %v", err)
	}
	t.Setenv("NIMLET_DB_MAX_CONNS", "")
	t.Setenv("NIMLET_MEDIA_MAX_BYTES", "50MB")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "NIMLET_MEDIA_MAX_BYTES") {
		t.Fatalf("expected an error naming NIMLET_MEDIA_MAX_BYTES, got %v", err)
	}
}
