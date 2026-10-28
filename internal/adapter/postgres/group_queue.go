package postgres

import (
	"context"
	"sync"
	"time"
)

// GroupLimits shapes one group queue: how many requests share a statement, how long a
// worker holding a first request waits for more, and how many groups run at once.
type GroupLimits struct {
	// MaxItems caps the requests in one group; <= 1 disables grouping.
	MaxItems int
	// MaxWait: zero takes only what is already queued. Under load requests pile up while
	// the workers are busy, so groups still grow. A worker only waits after a group of more
	// than one request: a lone request on an idle server never pays MaxWait.
	MaxWait time.Duration
	// WaitAlways makes a lone request wait MaxWait too, so that requests sent together
	// reliably share a group (tests).
	WaitAlways bool
	// Workers is the number of groups in flight at once.
	Workers int
}

// groupQueue hands concurrent requests to workers that run them in groups.
type groupQueue[T any] struct {
	limits  GroupLimits
	reqs    chan T // unbuffered: a request is only handed over to a worker that takes it
	stop    chan struct{}
	stopped sync.WaitGroup
}

func newGroupQueue[T any](limits GroupLimits, run func([]T)) *groupQueue[T] {
	limits.MaxItems = max(limits.MaxItems, 1)
	limits.Workers = max(limits.Workers, 1)
	q := &groupQueue[T]{limits: limits, reqs: make(chan T), stop: make(chan struct{})}
	for range limits.Workers {
		q.stopped.Add(1)
		go q.worker(run)
	}
	return q
}

// submit hands r to a worker. It returns false, without an error, once the queue is
// closed: the caller then runs the request on its own.
func (q *groupQueue[T]) submit(ctx context.Context, r T) (bool, error) {
	select {
	case q.reqs <- r:
		return true, nil
	case <-q.stop:
		return false, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// close stops the workers once the groups they hold are done.
func (q *groupQueue[T]) close() {
	close(q.stop)
	q.stopped.Wait()
}

func (q *groupQueue[T]) worker(run func([]T)) {
	defer q.stopped.Done()
	group := make([]T, 0, q.limits.MaxItems)
	loaded := q.limits.WaitAlways
	for {
		select {
		case <-q.stop:
			return
		case r := <-q.reqs:
			group = append(group[:0], r)
		}
		group = q.fill(group, loaded)
		loaded = q.limits.WaitAlways || len(group) > 1
		run(group)
	}
}

// fill adds requests to group up to MaxItems. It waits up to MaxWait for them only when
// linger is set (the previous group had company); otherwise it takes what is queued.
func (q *groupQueue[T]) fill(group []T, linger bool) []T {
	if q.limits.MaxWait <= 0 || !linger {
		for len(group) < q.limits.MaxItems {
			select {
			case r := <-q.reqs:
				group = append(group, r)
			default:
				return group
			}
		}
		return group
	}
	timer := time.NewTimer(q.limits.MaxWait)
	defer timer.Stop()
	for len(group) < q.limits.MaxItems {
		select {
		case r := <-q.reqs:
			group = append(group, r)
		case <-timer.C:
			return group
		}
	}
	return group
}
