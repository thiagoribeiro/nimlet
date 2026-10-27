package sync_test

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	nimsync "github.com/thiagoribeiro/nimlet/internal/sync"
)

func TestWaiterRegistry_NotifyWakesWaiter(t *testing.T) {
	r := nimsync.NewWaiterRegistry()
	ch := r.Register("user1", nil)
	defer r.Unregister("user1", nil, ch)

	r.Notify("user1")

	select {
	case <-ch:
		// expected
	default:
		t.Fatal("expected channel to be notified")
	}
}

func TestWaiterRegistry_NotifyUnknownUserIsNoop(t *testing.T) {
	r := nimsync.NewWaiterRegistry()
	// Should not panic.
	r.Notify("nonexistent")
}

func TestWaiterRegistry_UnregisterRemovesChannel(t *testing.T) {
	r := nimsync.NewWaiterRegistry()
	ch := r.Register("user1", nil)
	r.Unregister("user1", nil, ch)

	assert.Equal(t, 0, r.WaiterCount("user1"))
}

func TestWaiterRegistry_MultipleWaitersForSameUser(t *testing.T) {
	r := nimsync.NewWaiterRegistry()
	ch1 := r.Register("user1", nil)
	ch2 := r.Register("user1", nil)
	defer r.Unregister("user1", nil, ch1)
	defer r.Unregister("user1", nil, ch2)

	r.Notify("user1")

	require.Len(t, []struct{}{<-ch1}, 1, "ch1 must be notified")
	require.Len(t, []struct{}{<-ch2}, 1, "ch2 must be notified")
}

func TestWaiterRegistry_ConcurrentRegisterNotifyUnregister(t *testing.T) {
	r := nimsync.NewWaiterRegistry()
	var wg sync.WaitGroup
	const goroutines = 100

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch := r.Register("user_concurrent", nil)
			r.Notify("user_concurrent")
			r.Unregister("user_concurrent", nil, ch)
		}()
	}
	wg.Wait()
	// No race, no panic — registry must be empty.
	assert.Equal(t, 0, r.WaiterCount("user_concurrent"))
}

func TestWaiterRegistry_NotificationNotLostWhenBuffered(t *testing.T) {
	r := nimsync.NewWaiterRegistry()
	ch := r.Register("user1", nil)
	defer r.Unregister("user1", nil, ch)

	// Notify twice — second send must be dropped (buffer=1), not block.
	r.Notify("user1")
	r.Notify("user1") // must not deadlock

	select {
	case <-ch:
	default:
		t.Fatal("notification was lost")
	}
}

func TestWaiterRegistry_CloseIsIdempotent(t *testing.T) {
	r := nimsync.NewWaiterRegistry()
	select {
	case <-r.Closed():
		t.Fatal("closed before Close")
	default:
	}
	r.Close()
	r.Close()
	select {
	case <-r.Closed():
	default:
		t.Fatal("expected Closed to be closed")
	}
}
