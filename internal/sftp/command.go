package sftp

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

// Command is a program running on the far end of a connection, started by
// Start. Stdin and Stdout are its standard streams; what it writes to standard
// error is kept, the last stderrLimit bytes of it, for the error Wait returns.
//
// It shares the connection the SFTP session runs over — one more channel on
// the same SSH client — so starting one costs no handshake, and a host that
// would refuse a second login does not refuse this.
type Command struct {
	Stdin  io.WriteCloser
	Stdout io.Reader

	session *ssh.Session
	stderr  *tailBuffer

	closeOnce sync.Once
}

// stderrLimit is how much of what a command says on standard error is kept. It
// is for an error message, and the end of the stream is where the reason is.
const stderrLimit = 4 << 10

// Start runs command on the far end through the login's shell, the way
// `ssh host command` does.
//
// A login that may only speak SFTP — an sshd with ForceCommand internal-sftp,
// a storage box, most NAS accounts — refuses this or runs something else in
// its place, and either way says so on the first read: callers are expected to
// hold a command to a protocol of their own rather than trust that the program
// they named is the one answering.
func (c *Client) Start(command string) (*Command, error) {
	if c == nil || c.closed.Load() || c.ssh == nil {
		return nil, errors.New("the connection is closed")
	}
	session, err := c.ssh.NewSession()
	if err != nil {
		return nil, fmt.Errorf("opening a session to run a command: %w", err)
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		session.Close()
		return nil, err
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		return nil, err
	}
	stderr := &tailBuffer{limit: stderrLimit}
	session.Stderr = stderr

	if err := session.Start(command); err != nil {
		session.Close()
		return nil, fmt.Errorf("this login cannot run commands: %w", err)
	}
	return &Command{Stdin: stdin, Stdout: stdout, session: session, stderr: stderr}, nil
}

// Wait waits for the command to end, and answers nil only for an exit status
// of zero. Anything else carries what the command said on standard error.
func (c *Command) Wait() error {
	err := c.session.Wait()
	if err == nil {
		return nil
	}
	said := c.Stderr()
	var exit *ssh.ExitError
	switch {
	case errors.As(err, &exit) && said != "":
		return fmt.Errorf("exit status %d: %s", exit.ExitStatus(), said)
	case said != "":
		return fmt.Errorf("%w: %s", err, said)
	}
	return err
}

// Stderr is what the command has said on standard error so far, trimmed.
func (c *Command) Stderr() string { return strings.TrimSpace(c.stderr.String()) }

// Close ends the command: its input is closed, it is asked to stop, and the
// channel is torn down whether or not it listens. A program reading its input
// sees the end of it; one that only writes sees its output go away. Safe to
// call more than once, and after Wait.
func (c *Command) Close() error {
	var err error
	c.closeOnce.Do(func() {
		_ = c.Stdin.Close()
		// Best effort: OpenSSH honours signals from 7.9 on, and older
		// servers ignore the request rather than failing it.
		_ = c.session.Signal(ssh.SIGTERM)
		err = c.session.Close()
		if errors.Is(err, io.EOF) {
			err = nil
		}
	})
	return err
}

// tailBuffer keeps the last limit bytes written to it.
type tailBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.limit; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
