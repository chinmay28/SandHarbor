package hostenv

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestParseServers(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"1.1.1.1", []string{"1.1.1.1:53"}},
		{"1.1.1.1, 8.8.8.8:5353", []string{"1.1.1.1:53", "8.8.8.8:5353"}},
		{"2606:4700::1111", []string{"[2606:4700::1111]:53"}},
		{"[2606:4700::1111]:853", []string{"[2606:4700::1111]:853"}},
		{"[2606:4700::1111]", []string{"[2606:4700::1111]:53"}},
		{"::ffff:10.0.0.1", []string{"10.0.0.1:53"}},
		{"# the phone's servers\n192.168.1.1  # router\n\n10.0.0.1\r\n", []string{"192.168.1.1:53", "10.0.0.1:53"}},
		{"1.1.1.1 1.1.1.1:53", []string{"1.1.1.1:53"}},
		{"dns.google, 1.1.1.1:0, fe80::1%wlan0, 9.9.9.9", []string{"9.9.9.9:53"}},
	}
	for _, c := range cases {
		if got := ParseServers(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseServers(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFileServersFollowsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dns")
	now := time.Unix(1000, 0)
	src := newFileSource(path, StaticServers("9.9.9.9"), time.Second, func() time.Time { return now })

	if got := src.servers(); !reflect.DeepEqual(got, []string{"9.9.9.9:53"}) {
		t.Fatalf("missing file: got %q, want the fallback", got)
	}

	write := func(text string, mtime time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}

	write("192.168.1.1\n", time.Unix(2000, 0))
	if got := src.servers(); !reflect.DeepEqual(got, []string{"9.9.9.9:53"}) {
		t.Fatalf("within the recheck interval: got %q, want the old answer", got)
	}
	now = now.Add(time.Second)
	if got := src.servers(); !reflect.DeepEqual(got, []string{"192.168.1.1:53"}) {
		t.Fatalf("after the interval: got %q", got)
	}

	// A new network: the app rewrites the file.
	write("10.0.0.1, 10.0.0.2\n", time.Unix(3000, 0))
	now = now.Add(time.Second)
	if got := src.servers(); !reflect.DeepEqual(got, []string{"10.0.0.1:53", "10.0.0.2:53"}) {
		t.Fatalf("after a rewrite: got %q", got)
	}

	// No network: the file names nothing, so the fallback answers.
	write("", time.Unix(4000, 0))
	now = now.Add(time.Second)
	if got := src.servers(); !reflect.DeepEqual(got, []string{"9.9.9.9:53"}) {
		t.Fatalf("empty file: got %q, want the fallback", got)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if got := src.servers(); !reflect.DeepEqual(got, []string{"9.9.9.9:53"}) {
		t.Fatalf("removed file: got %q, want the fallback", got)
	}
}

func TestFileServersWithNoFallback(t *testing.T) {
	src := FileServers(filepath.Join(t.TempDir(), "absent"), nil)
	if got := src(); len(got) != 0 {
		t.Fatalf("got %q, want nothing", got)
	}
}

// fakeDNS answers every A question with ip, and counts what it was asked.
func fakeDNS(t *testing.T, ip [4]byte) (addr string, asked *atomic.Int32) {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	asked = new(atomic.Int32)

	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			var p dnsmessage.Parser
			hdr, err := p.Start(buf[:n])
			if err != nil {
				continue
			}
			q, err := p.Question()
			if err != nil {
				continue
			}
			asked.Add(1)
			b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: hdr.ID, Response: true, Authoritative: true, RecursionDesired: hdr.RecursionDesired})
			b.EnableCompression()
			_ = b.StartQuestions()
			_ = b.Question(q)
			_ = b.StartAnswers()
			if q.Type == dnsmessage.TypeA {
				_ = b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 60}, dnsmessage.AResource{A: ip})
			}
			out, err := b.Finish()
			if err != nil {
				continue
			}
			_, _ = conn.WriteTo(out, from)
		}
	}()
	return conn.LocalAddr().String(), asked
}

func TestResolverAsksTheConfiguredServer(t *testing.T) {
	addr, asked := fakeDNS(t, [4]byte{192, 0, 2, 7})
	r := NewResolver(StaticServers(addr))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ips, err := r.LookupHost(ctx, "vault.example.test.")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ips, []string{"192.0.2.7"}) {
		t.Fatalf("got %q", ips)
	}
	if asked.Load() == 0 {
		t.Fatal("the fake server was never asked")
	}
}

func TestResolverTakesTheServersInTurn(t *testing.T) {
	a, askedA := fakeDNS(t, [4]byte{192, 0, 2, 1})
	b, askedB := fakeDNS(t, [4]byte{192, 0, 2, 2})
	r := NewResolver(StaticServers(a + "," + b))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 0; i < 4; i++ {
		if _, err := r.LookupIPAddr(ctx, "vault.example.test."); err != nil {
			t.Fatal(err)
		}
	}
	if askedA.Load() == 0 || askedB.Load() == 0 {
		t.Fatalf("queries went to one server only: a=%d b=%d", askedA.Load(), askedB.Load())
	}
}

func TestResolverWithNoServersFails(t *testing.T) {
	r := NewResolver(func() []string { return nil })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := r.LookupHost(ctx, "vault.example.test."); err == nil {
		t.Fatal("a lookup with no servers succeeded")
	}
}
