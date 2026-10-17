package sync

import (
	"context"
	"log/slog"
	"time"

	"github.com/thiagoribeiro/nimlet/internal/port/out"
)

const defaultCoalesceWindow = 10 * time.Millisecond

// Dispatcher bridges the EventBus to the WaiterRegistry.
// It subscribes to every partition of the bus and fans notifications
// out to registered waiters via a coalescer per partition.
type Dispatcher struct {
	bus      out.EventBus
	registry *WaiterRegistry
	window   time.Duration
}

func NewDispatcher(bus out.EventBus, registry *WaiterRegistry) *Dispatcher {
	return &Dispatcher{
		bus:      bus,
		registry: registry,
		window:   defaultCoalesceWindow,
	}
}

// Run starts one goroutine per bus partition and blocks until ctx is cancelled.
// It must be started before the HTTP server to avoid missing notifications.
func (d *Dispatcher) Run(ctx context.Context) {
	n := d.bus.NumPartitions()
	done := make(chan struct{})

	for p := 0; p < n; p++ {
		go d.runPartition(ctx, p, done)
	}

	// Wait for all partition goroutines to exit.
	for i := 0; i < n; i++ {
		<-done
	}
}

func (d *Dispatcher) runPartition(ctx context.Context, partition int, done chan<- struct{}) {
	defer func() { done <- struct{}{} }()

	ch, cancel, err := d.bus.Subscribe(ctx, partition)
	if err != nil {
		slog.ErrorContext(ctx, "dispatcher: subscribe failed",
			"partition", partition, "error", err)
		return
	}
	defer cancel()

	c := newCoalescer(d.window, d.registry)

	for {
		select {
		case <-ctx.Done():
			return
		case key, ok := <-ch:
			if !ok {
				return
			}
			c.add(key)
		}
	}
}
