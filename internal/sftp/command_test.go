package sftp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/chinmay28/SandHarbor/internal/sftp/sftptest"
)

func dialExec(t *testing.T, exec func(string, io.Reader, io.Writer, io.Writer) int) *Client {
	t.Helper()
	server := sftptest.NewServer(t, t.TempDir())
	server.Exec = exec
	key, _ := sftptest.NewClientKey(t)
	client, err := Dial(context.Background(), dialTest(t, server, key, ""))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

// A command's streams are the program's: what goes in on stdin comes back on
// stdout, and the command line arrives as it was written.
func TestStartRunsACommandOverTheConnection(t *testing.T) {
	client := dialExec(t, func(command string, stdin io.Reader, stdout, _ io.Writer) int {
		fmt.Fprintf(stdout, "ran %s\n", command)
		io.Copy(stdout, stdin)
		return 0
	})

	cmd, err := client.Start("sand grind")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	io.WriteString(cmd.Stdin, "echoed\n")
	cmd.Stdin.Close()

	out, _ := io.ReadAll(cmd.Stdout)
	if got := string(out); got != "ran sand grind\nechoed\n" {
		t.Fatalf("stdout = %q", got)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

// A command that fails says why, in its own words: "sand: not found" is the
// whole diagnosis of a machine without the binary.
func TestWaitCarriesStderrOnFailure(t *testing.T) {
	client := dialExec(t, func(_ string, _ io.Reader, _, stderr io.Writer) int {
		fmt.Fprintln(stderr, "sh: sand: not found")
		return 127
	})

	cmd, err := client.Start("sand grind")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	io.ReadAll(cmd.Stdout)
	err = cmd.Wait()
	if err == nil || !strings.Contains(err.Error(), "127") || !strings.Contains(err.Error(), "sand: not found") {
		t.Fatalf("Wait = %v, want the status and the message", err)
	}
}

// Closing a command is how the far end learns nobody is listening: its input
// ends, so a program that reads it stops rather than running on.
func TestCloseEndsTheCommandsInput(t *testing.T) {
	ended := make(chan struct{})
	client := dialExec(t, func(_ string, stdin io.Reader, stdout, _ io.Writer) int {
		fmt.Fprintln(stdout, "ready")
		io.Copy(io.Discard, stdin)
		close(ended)
		return 0
	})

	cmd, err := client.Start("sand grind")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if line, _ := bufio.NewReader(cmd.Stdout).ReadString('\n'); line != "ready\n" {
		t.Fatalf("first line = %q", line)
	}
	if err := cmd.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	<-ended
	if err := cmd.Close(); err != nil {
		t.Fatalf("a second Close: %v", err)
	}
}

// A login that may only speak SFTP refuses exec, and says so as a command
// that could not start rather than as a protocol error later.
func TestStartOnAnSFTPOnlyLogin(t *testing.T) {
	client := dialExec(t, nil)
	if _, err := client.Start("sand grind"); err == nil || !strings.Contains(err.Error(), "cannot run commands") {
		t.Fatalf("Start = %v, want a refusal", err)
	}
}

func TestTailBufferKeepsTheEnd(t *testing.T) {
	b := &tailBuffer{limit: 5}
	b.Write([]byte("abc"))
	b.Write([]byte("defgh"))
	if got := b.String(); got != "defgh" {
		t.Fatalf("tail = %q, want %q", got, "defgh")
	}
}
