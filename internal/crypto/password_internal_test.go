package crypto

import (
	"errors"
	"testing"
	"time"
)

// Concurrent hashing never exceeds the slot count, and callers that cannot get a slot in
// time fail with ErrBusy instead of piling up memory.
func TestHashSlots_BoundConcurrency(t *testing.T) {
	saved := hashQueueTimeout
	hashQueueTimeout = 20 * time.Millisecond
	defer func() { hashQueueTimeout = saved }()

	// Hold every slot, then ask for one more.
	for range cap(hashSlots) {
		hashSlots <- struct{}{}
	}
	_, err := HashPassword("pw")
	for range cap(hashSlots) {
		<-hashSlots
	}
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy with all slots taken, got %v", err)
	}
	if _, err := HashPassword("pw"); err != nil {
		t.Fatalf("slot free again: %v", err)
	}
}
