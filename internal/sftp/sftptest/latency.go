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
			go func() { defer wg.Done(); delayed(up, down, rtt/2) }()
			go func() { defer wg.Done(); delayed(down, up, rtt/2) }()
		}
	}()
	return l.Addr().String()
}

// delayed copies src to dst, writing each chunk read no sooner than oneWay
// after it was read, and closes dst when src ends.
func delayed(dst, src net.Conn, oneWay time.Duration) {
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
	for c := range queue {
		time.Sleep(time.Until(c.due))
		if _, err := dst.Write(c.b); err != nil {
			break
		}
	}
	dst.Close()
	src.Close()
	// Drain so the reader is never left blocked on a full queue.
	for range queue {
	}
}
