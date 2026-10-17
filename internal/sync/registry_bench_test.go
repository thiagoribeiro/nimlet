package sync

import (
	"fmt"
	"testing"
)

// BenchmarkRegistry_Churn models long-poll reconnects in a large room that already has
// n waiters: one Register+Unregister pair per iteration.
func BenchmarkRegistry_Churn(b *testing.B) {
	for _, n := range []int{10, 1000, 10000} {
		b.Run(fmt.Sprintf("waiters=%d", n), func(b *testing.B) {
			r := NewWaiterRegistry()
			rooms := []string{"room:large"}
			for i := range n {
				r.Register(fmt.Sprintf("user-%d", i), rooms)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				ch := r.Register("churner", rooms)
				r.Unregister("churner", rooms, ch)
			}
		})
	}
}

// BenchmarkRegistry_ChurnOldest unregisters the earliest waiter, the worst case for a
// linear scan from the slice head.
func BenchmarkRegistry_ChurnOldest(b *testing.B) {
	const n = 10000
	r := NewWaiterRegistry()
	rooms := []string{"room:large"}
	type waiter struct {
		user string
		ch   chan struct{}
	}
	ws := make([]waiter, n)
	for i := range ws {
		ws[i].user = fmt.Sprintf("user-%d", i)
		ws[i].ch = r.Register(ws[i].user, rooms)
	}
	b.ReportAllocs()
	b.ResetTimer()
	i := 0
	for b.Loop() {
		w := ws[i%n]
		r.Unregister(w.user, rooms, w.ch)
		ws[i%n].ch = r.Register(w.user, rooms)
		i++
	}
}

func BenchmarkRegistry_NotifyRoom(b *testing.B) {
	for _, n := range []int{10, 1000, 10000} {
		b.Run(fmt.Sprintf("waiters=%d", n), func(b *testing.B) {
			r := NewWaiterRegistry()
			rooms := []string{"room:large"}
			chans := make([]chan struct{}, n)
			for i := range chans {
				chans[i] = r.Register(fmt.Sprintf("user-%d", i), rooms)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				r.NotifyRoom("room:large")
				for _, ch := range chans {
					<-ch
				}
			}
		})
	}
}

func BenchmarkRegistry_NotifyUserParallel(b *testing.B) {
	r := NewWaiterRegistry()
	const users = 4096
	ids := make([]string, users)
	for i := range ids {
		ids[i] = fmt.Sprintf("user-%d", i)
		r.Register(ids[i], nil)
	}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			r.Notify(ids[i%users])
			i++
		}
	})
}
