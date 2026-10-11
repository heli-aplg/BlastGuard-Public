// Package tunnel checks local ports and runs the configured ssh command.
package tunnel

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Listening reports whether something accepts TCP connections on addr.
func Listening(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// Tunnel is a tunnel process started by BlastGuard. It lives as long as the
// tool: Close stops it.
type Tunnel struct {
	cmd  *exec.Cmd
	done chan struct{}
}

// Start runs command (e.g. "ssh -L 6432:127.0.0.1:5432 host -N") as a child
// process and waits until addr accepts connections. stdin is /dev/null so the
// process never steals keystrokes from the TUI; ssh still asks for passwords
// and passphrases on /dev/tty. stdout/stderr are captured instead of printed
// so a late ssh message cannot garble the TUI; they are shown on failure.
// Commands that background themselves (ssh -f) also work.
func Start(ctx context.Context, command, addr string, wait time.Duration) (*Tunnel, error) {
	// exec: the shell is replaced by the command, so killing the child kills ssh.
	cmd := exec.Command("/bin/sh", "-c", "exec "+command)
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	t := &Tunnel{cmd: cmd, done: make(chan struct{})}
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(t.done) }()

	deadline := time.After(wait)
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	exited := false
	for {
		select {
		case <-t.done:
			if !exited {
				exited = true
				if waitErr != nil {
					return nil, fmt.Errorf("tunnel command failed: %v%s", waitErr, tail(out.String()))
				}
				// exited cleanly: it backgrounded itself (ssh -f); keep polling.
			}
		case <-tick.C:
			if Listening(addr) {
				return t, nil
			}
		case <-deadline:
			t.Close()
			return nil, fmt.Errorf("%s is still not listening after %s%s", addr, wait, tail(out.String()))
		case <-ctx.Done():
			t.Close()
			return nil, ctx.Err()
		}
	}
}

// Close stops the tunnel process if it is still running. Safe on nil.
func (t *Tunnel) Close() {
	if t == nil {
		return
	}
	select {
	case <-t.done:
		return
	default:
	}
	_ = t.cmd.Process.Kill()
	<-t.done
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) > 500 {
		s = "…" + s[len(s)-500:]
	}
	return "\n" + s
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
