package sftptest

import (
	"net"
	"sync"
	"testing"
	"time"
)

// Latency starts a TCP proxy in front of upstream that holds every byte for
// half of rtt in each direction, and returns the address to dial instead.
//
// It is for the bugs that only exist at a distance. On localhost a round trip
// is free, so code that makes one per packet runs as fast as code that does
// not, and a test there cannot tell them apart; behind this proxy it can. Only
// delay is added — bandwidth is whatever the machine running the test has —
// so what a test measures through it is how well the code hides round trips.
//
// The proxy and every connection through it stop when the test ends.
func Latency(t testing.TB, upstream string, rtt time.Duration) string {
	t.Helper()
	return Link(t, upstream, rtt, 0)
}

// Link is Latency with each connection through it also held to perConnection
// bytes a second in each direction; zero is no limit.
//
// The limit is per connection, not shared, which is the shape that matters: a
// path that loses packets holds each TCP flow to what its congestion window
// recovers to, whatever the link is, and a test on localhost — where nothing
// is lost — can only stand that in by capping the flow. Code that spreads a
// transfer over several connections goes faster through this; code that
// spreads it over several sessions on one connection does not.
func Link(t testing.TB, upstream string, rtt time.Duration, perConnection int64) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	var (
		mu    sync.Mutex
		conns []net.Conn
		wg    sync.WaitGroup
	)
	t.Cleanup(func() {
		l.Close()
		mu.Lock()
		for _, c := range conns {
			c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			down, err := l.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", upstream)
			if err != nil {
				down.Close()
				continue
			}
			mu.Lock()
			conns = append(conns, down, up)
			mu.Unlock()
			wg.Add(2)
			go func() { defer wg.Done(); delayed(up, down, rtt/2, perConnection) }()
			go func() { defer wg.Done(); delayed(down, up, rtt/2, perConnection) }()
		}
	}()
	return l.Addr().String()
}

// delayed copies src to dst, writing each chunk read no sooner than oneWay
// after it was read and, with a rate, no faster than rate bytes a second, and
// closes dst when src ends.
func delayed(dst, src net.Conn, oneWay time.Duration, rate int64) {
	type chunk struct {
		due time.Time
		b   []byte
	}
	// Deep enough that the queue is never what limits throughput: a megabyte
	// chunk every millisecond still fits.
	queue := make(chan chunk, 1<<14)
	go func() {
		defer close(queue)
		for {
			b := make([]byte, 64<<10)
			n, err := src.Read(b)
			if n > 0 {
				queue <- chunk{due: time.Now().Add(oneWay), b: b[:n]}
			}
			if err != nil {
				return
			}
		}
	}()
	// free is when the link has finished sending what it was last given.
	var free time.Time
	for c := range queue {
		at := c.due
		if rate > 0 && free.After(at) {
			at = free
		}
		time.Sleep(time.Until(at))
		if _, err := dst.Write(c.b); err != nil {
			break
		}
		if rate > 0 {
			free = at.Add(time.Duration(int64(len(c.b)) * int64(time.Second) / rate))
		}
	}
	dst.Close()
	src.Close()
	// Drain so the reader is never left blocked on a full queue.
	for range queue {
	}
}
