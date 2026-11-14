package hostenv

import (
	"context"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Servers returns the DNS servers to ask, each as "ip:port". It is called on
// every query, so it must be cheap; an empty answer fails the query.
type Servers func() []string

// NewResolver is a pure-Go resolver that sends every query to the servers
// returned by servers, whatever the host's own configuration says.
//
// Go's resolver still decides how to ask — retries, timeouts, falling back
// to TCP for a truncated answer — and hands each attempt to Dial with the
// server it would have used. Dial ignores that address and takes the next of
// ours in turn, so a retry after a timeout goes to a different server.
func NewResolver(servers Servers) *net.Resolver {
	var next atomic.Uint64
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			list := servers()
			if len(list) == 0 {
				return nil, &net.DNSError{Err: "no DNS servers configured", IsTemporary: true}
			}
			addr := list[int((next.Add(1)-1)%uint64(len(list)))]
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
}

// ParseServers reads a server list: IP addresses, with or without a port,
// separated by commas, spaces or newlines. A '#' starts a comment that runs
// to the end of its line. Anything that is not an address is skipped, and so
// is a repeat. Each address comes back as "ip:port", port 53 when none was
// given; an IPv6 address with a port is written "[2001:db8::1]:53".
func ParseServers(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		for _, field := range strings.FieldsFunc(line, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t' || r == '\r'
		}) {
			addr, ok := parseServer(field)
			if ok && !seen[addr] {
				seen[addr] = true
				out = append(out, addr)
			}
		}
	}
	return out
}

func parseServer(s string) (string, bool) {
	if ap, err := netip.ParseAddrPort(s); err == nil {
		if ap.Port() == 0 {
			return "", false
		}
		return ap.String(), true
	}
	ip, err := netip.ParseAddr(strings.Trim(s, "[]"))
	if err != nil || ip.Zone() != "" {
		// A zoned address (fe80::1%wlan0) names an interface the server
		// process may not see the same way; Android hands those out only
		// alongside routable ones, so leaving it out loses nothing.
		return "", false
	}
	return netip.AddrPortFrom(ip.Unmap(), 53).String(), true
}

// StaticServers is a fixed list, parsed once.
func StaticServers(list string) Servers {
	parsed := ParseServers(list)
	return func() []string { return parsed }
}

// FileServers reads the list from path, again whenever the file's size or
// modification time changes, and at most once every recheck interval
// otherwise checks nothing. While the file is missing, unreadable or names
// no server, the answer is fallback's.
func FileServers(path string, fallback Servers) Servers {
	return newFileSource(path, fallback, time.Second, time.Now).servers
}

type fileSource struct {
	path     string
	fallback Servers
	recheck  time.Duration
	now      func() time.Time

	mu      sync.Mutex
	checked time.Time
	modTime time.Time
	size    int64
	list    []string
}

func newFileSource(path string, fallback Servers, recheck time.Duration, now func() time.Time) *fileSource {
	if fallback == nil {
		fallback = func() []string { return nil }
	}
	return &fileSource{path: path, fallback: fallback, recheck: recheck, now: now}
}

func (f *fileSource) servers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	if now := f.now(); f.checked.IsZero() || now.Sub(f.checked) >= f.recheck {
		f.checked = now
		f.refresh()
	}
	if len(f.list) > 0 {
		return f.list
	}
	return f.fallback()
}

func (f *fileSource) refresh() {
	info, err := os.Stat(f.path)
	if err != nil {
		f.list, f.modTime, f.size = nil, time.Time{}, 0
		return
	}
	if info.ModTime().Equal(f.modTime) && info.Size() == f.size && f.list != nil {
		return
	}
	data, err := os.ReadFile(f.path)
	if err != nil {
		f.list = nil
		return
	}
	f.modTime, f.size = info.ModTime(), info.Size()
	f.list = ParseServers(string(data))
}
