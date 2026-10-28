package postgres

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestGroupQueue_LoneRequestSkipsWait(t *testing.T) {
	groups := make(chan int, 16)
	q := newGroupQueue(GroupLimits{MaxItems: 100, MaxWait: time.Second, Workers: 1}, func(g []int) {
		groups <- len(g)
	})
	defer q.close()

	start := time.Now()
	if ok, err := q.submit(context.Background(), 1); !ok || err != nil {
		t.Fatalf("submit: ok=%v err=%v", ok, err)
	}
	if n := <-groups; n != 1 {
		t.Fatalf("group size = %d, want 1", n)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("lone request waited %v, want no MaxWait", d)
	}
}

func TestGroupQueue_WaitsAfterLoadedGroup(t *testing.T) {
	release := make(chan struct{})
	groups := make(chan int, 16)
	first := true
	q := newGroupQueue(GroupLimits{MaxItems: 100, MaxWait: 200 * time.Millisecond, Workers: 1}, func(g []int) {
		if first {
			first = false
			<-release // hold the worker so the next requests pile up
		}
		groups <- len(g)
	})
	defer q.close()

	submit := func(v int) {
		if ok, err := q.submit(context.Background(), v); !ok || err != nil {
			t.Errorf("submit: ok=%v err=%v", ok, err)
		}
	}
	submit(0) // lone: runs at once and blocks the worker
	var wg sync.WaitGroup
	for i := 1; i <= 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); submit(i) }()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := <-groups; n != 1 {
		t.Fatalf("first group = %d, want 1", n)
	}
	// The worker takes the three queued requests as they hand over; the group of 3 (or
	// whatever was ready) marks it loaded, so the next lone request lingers for MaxWait.
	got := 0
	for got < 3 {
		got += <-groups
	}
	start := time.Now()
	go submit(4)
	time.Sleep(20 * time.Millisecond)
	go submit(5)
	n := <-groups
	if d := time.Since(start); n != 2 || d < 150*time.Millisecond {
		t.Fatalf("after load: group of %d after %v, want 2 after ~MaxWait", n, d)
	}
}
