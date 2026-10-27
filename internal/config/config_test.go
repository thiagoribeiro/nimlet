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

func TestLoadDB_Profiles(t *testing.T) {
	t.Setenv("NIMLET_DB_DSN", "postgres://x")

	t.Setenv("NIMLET_PROFILE", "small")
	db, err := LoadDB()
	if err != nil {
		t.Fatal(err)
	}
	if db.EventBatchWorkers != 1 || db.SyncReadWorkers != 1 || db.SyncReadWaitUs != 2000 || db.MaxConns != 12 {
		t.Fatalf("small profile not applied: %+v", db)
	}

	// An explicit variable overrides the profile.
	t.Setenv("NIMLET_SYNC_READ_WORKERS", "3")
	db, err = LoadDB()
	if err != nil {
		t.Fatal(err)
	}
	if db.SyncReadWorkers != 3 || db.EventBatchWorkers != 1 {
		t.Fatalf("override lost: %+v", db)
	}

	t.Setenv("NIMLET_PROFILE", "huge")
	if _, err := LoadDB(); err == nil {
		t.Fatal("unknown profile accepted")
	}
}

func TestLoadDB_EventBatchMode(t *testing.T) {
	t.Setenv("NIMLET_DB_DSN", "postgres://x")

	db, err := LoadDB()
	if err != nil {
		t.Fatal(err)
	}
	if db.EventBatchSingleTx {
		t.Fatal("default must be split")
	}

	t.Setenv("NIMLET_EVENT_BATCH_MODE", "single")
	if db, err = LoadDB(); err != nil || !db.EventBatchSingleTx {
		t.Fatalf("single not applied: %+v, %v", db, err)
	}

	t.Setenv("NIMLET_EVENT_BATCH_MODE", "both")
	if _, err := LoadDB(); err == nil {
		t.Fatal("expected an error for an unknown mode")
	}
}
