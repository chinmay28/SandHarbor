package sftp

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/chinmay28/SandHarbor/internal/sftp/sftptest"
)

// stripeFixture serves one file and returns a client for it, cut up finely
// enough that a file of a few hundred kilobytes is spread across sessions.
func stripeFixture(t *testing.T, data []byte, streams int, tweak func(*sftptest.Server)) (*Client, *sftptest.Server, string) {
	t.Helper()
	disk := t.TempDir()
	if err := os.WriteFile(filepath.Join(disk, "film.mp4"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	server := sftptest.NewServer(t, disk)
	if tweak != nil {
		tweak(server)
	}
	host, port := server.HostPort(t)
	key, _ := sftptest.NewClientKey(t)
	client, err := Dial(context.Background(), Config{
		Host: host, Port: port, User: "sand", PrivateKey: key, Streams: streams,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	// Small enough that a file of a few hundred kilobytes is spread across
	// every session and comes out in many pieces.
	client.block, client.inFlight, client.stripeMin = 4<<10, 4, 64<<10
	return client, server, filepath.ToSlash(disk)
}

func copyOut(t *testing.T, c *Client, root string, skip int) []byte {
	t.Helper()
	f, _, err := c.OpenUnder(root, "film.mp4")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	if skip > 0 {
		if _, err := io.ReadFull(f, make([]byte, skip)); err != nil {
			t.Fatalf("reading the first %d bytes: %v", skip, err)
		}
	}
	var out bytes.Buffer
	n, err := io.Copy(&out, f)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if n != int64(out.Len()) {
		t.Errorf("copy reported %d bytes, wrote %d", n, out.Len())
	}
	return out.Bytes()
}

func TestRemoteFileCopiesALargeFileAcrossSessions(t *testing.T) {
	data := patterned(10*(32<<10) + 123)
	client, server, root := stripeFixture(t, data, 4, nil)

	if got := copyOut(t, client, root, 0); !bytes.Equal(got, data) {
		t.Fatalf("copied %d bytes, not the file's %d", len(got), len(data))
	}
	// The primary session plus three more.
	if got := server.SessionsOpened(); got != 4 {
		t.Errorf("%d sessions opened, want 4", got)
	}

	// A second file reuses them rather than paying to open them again.
	if got := copyOut(t, client, root, 0); !bytes.Equal(got, data) {
		t.Fatal("the second copy differs")
	}
	if got := server.SessionsOpened(); got != 4 {
		t.Errorf("%d sessions opened after a second file, want them reused", got)
	}
}

func TestRemoteFileCopiesFromWhereReadingLeftOff(t *testing.T) {
	data := patterned(10 * (32 << 10))
	client, _, root := stripeFixture(t, data, 4, nil)

	const skip = 32<<10 + 5
	if got := copyOut(t, client, root, skip); !bytes.Equal(got, data[skip:]) {
		t.Fatalf("copied %d bytes, want the %d after the first %d", len(got), len(data)-skip, skip)
	}
}

func TestRemoteFileUsesWhatAStingyServerAllows(t *testing.T) {
	data := patterned(10 * (32 << 10))
	for _, max := range []int{1, 2, 3} {
		t.Run(strconv.Itoa(max), func(t *testing.T) {
			client, server, root := stripeFixture(t, data, 8, func(s *sftptest.Server) { s.MaxSessions = max })

			if got := copyOut(t, client, root, 0); !bytes.Equal(got, data) {
				t.Fatal("copy differs")
			}
			if got := server.SessionsOpened(); got != max {
				t.Errorf("%d sessions opened, want the %d allowed", got, max)
			}
			// A refusal is remembered rather than met with a fresh attempt
			// every file; the connection still works afterwards.
			if got := copyOut(t, client, root, 0); !bytes.Equal(got, data) {
				t.Fatal("second copy differs")
			}
			if _, err := client.SFTP().Getwd(); err != nil {
				t.Errorf("the primary session was hurt by the refusals: %v", err)
			}
		})
	}
}

func TestRemoteFileLeavesSmallFilesAndSingleStreamsAlone(t *testing.T) {
	small := patterned(64<<10 - 1)
	client, server, root := stripeFixture(t, small, 8, nil)
	if got := copyOut(t, client, root, 0); !bytes.Equal(got, small) {
		t.Fatal("small copy differs")
	}
	if got := server.SessionsOpened(); got != 1 {
		t.Errorf("a file under the striping minimum opened %d sessions, want just the primary", got)
	}

	large := patterned(10 * (32 << 10))
	client, server, root = stripeFixture(t, large, 1, nil)
	if got := copyOut(t, client, root, 0); !bytes.Equal(got, large) {
		t.Fatal("single-stream copy differs")
	}
	if got := server.SessionsOpened(); got != 1 {
		t.Errorf("Streams: 1 opened %d sessions", got)
	}
}

func TestRemoteFileCopiesAFileThatGrewWhole(t *testing.T) {
	data := patterned(10 * (32 << 10))
	client, _, root := stripeFixture(t, data, 4, nil)

	f, _, err := client.OpenUnder(root, "film.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	grown := append(append([]byte{}, data...), patterned(1000)...)
	if err := os.WriteFile(filepath.Join(root, "film.mp4"), grown, 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := io.Copy(&out, f); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), grown) {
		t.Errorf("copied %d bytes, want all %d of the grown file", out.Len(), len(grown))
	}
}

func TestClientCloseClosesExtraSessions(t *testing.T) {
	data := patterned(10 * (32 << 10))
	client, _, root := stripeFixture(t, data, 4, nil)
	copyOut(t, client, root, 0)

	extras := client.extraSessions()
	if len(extras) != 3 {
		t.Fatalf("%d extra sessions, want 3", len(extras))
	}
	client.Close()
	for i, s := range extras {
		if _, err := s.Getwd(); err == nil {
			t.Errorf("extra session %d still answers after Close", i)
		}
	}
	if got := client.extraSessions(); got != nil {
		t.Errorf("a closed client handed out %d sessions", len(got))
	}
}

// The reason any of this exists, measured: at a distance, striping is several
// times faster than one session. Skipped under -short because it moves tens
// of megabytes through a delaying proxy.
func TestRemoteFileIsFasterAtADistance(t *testing.T) {
	if testing.Short() {
		t.Skip("moves tens of megabytes through a delaying proxy")
	}
	const rtt = 40 * time.Millisecond
	data := patterned(48 << 20)
	disk := t.TempDir()
	if err := os.WriteFile(filepath.Join(disk, "film.mp4"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	server := sftptest.NewServer(t, disk)
	far := sftptest.Latency(t, server.Addr, rtt)
	host, portText, _ := net.SplitHostPort(far)
	port, _ := strconv.Atoi(portText)
	key, _ := sftptest.NewClientKey(t)

	timed := func(streams int) time.Duration {
		client, err := Dial(context.Background(), Config{
			Host: host, Port: port, User: "sand", PrivateKey: key,
			HostKey: server.Fingerprint, Streams: streams,
		})
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer client.Close()
		client.extraSessions() // opened up front, so only the copy is timed

		f, _, err := client.OpenUnder(filepath.ToSlash(disk), "film.mp4")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		start := time.Now()
		n, err := io.Copy(io.Discard, f)
		if err != nil || n != int64(len(data)) {
			t.Fatalf("copy: %d bytes, %v", n, err)
		}
		return time.Since(start)
	}

	one, four := timed(1), timed(4)
	mbps := func(d time.Duration) float64 { return float64(len(data)) / d.Seconds() / 1e6 }
	t.Logf("at %v: one session %.1f MB/s, four %.1f MB/s", rtt, mbps(one), mbps(four))
	if float64(one)/float64(four) < 1.5 {
		t.Errorf("four sessions took %v against one's %v: striping is not paying for itself", four, one)
	}
}

// pieceWriter records the largest single write it is handed.
type pieceWriter struct {
	bytes.Buffer
	largest int
}

func (p *pieceWriter) Write(b []byte) (int, error) {
	p.largest = max(p.largest, len(b))
	return p.Buffer.Write(b)
}

// What reads the file sees it arrive a packet at a time, as it would from a
// single session — a progress counter on a slow link must not wait for
// megabytes to land in one piece before it can say anything.
func TestRemoteFileHandsTheFileOnAPacketAtATime(t *testing.T) {
	data := patterned(10 * (32 << 10))
	client, server, root := stripeFixture(t, data, 4, nil)

	f, _, err := client.OpenUnder(root, "film.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out pieceWriter
	if _, err := io.Copy(&out, f); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), data) {
		t.Fatal("copy differs")
	}
	if server.SessionsOpened() != 4 {
		t.Fatalf("%d sessions opened, so this did not test striping", server.SessionsOpened())
	}
	if out.largest > client.block {
		t.Errorf("a %d byte write, larger than the %d byte block", out.largest, client.block)
	}
}
