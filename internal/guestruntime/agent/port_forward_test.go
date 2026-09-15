//go:build linux

package guestagent

import (
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

// bridgeFixture stands up bridgePortForward between one end of a unix
// socketpair (standing in for the vsock; the host proxy reads the other end)
// and the client end of a real TCP connection (standing in for the guest
// service the agent dialed).
type bridgeFixture struct {
	host     *os.File
	backend  net.Conn
	returned chan struct{}
}

func newBridgeFixture(t *testing.T) *bridgeFixture {
	t.Helper()

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	host := os.NewFile(uintptr(fds[1]), "host")
	t.Cleanup(func() { host.Close() })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- c
	}()
	target, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	backend := <-accepted
	if backend == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { backend.Close() })

	f := &bridgeFixture{host: host, backend: backend, returned: make(chan struct{})}
	go func() {
		bridgePortForward(fds[0], target)
		close(f.returned)
	}()
	return f
}

func (f *bridgeFixture) waitReturned(t *testing.T, why string) {
	t.Helper()
	select {
	case <-f.returned:
	case <-time.After(5 * time.Second):
		t.Fatalf("bridgePortForward never returned after %s", why)
	}
}

// readAllWithin reads r to EOF, failing the test if EOF does not arrive in time.
func readAllWithin(t *testing.T, r io.Reader, why string) []byte {
	t.Helper()
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(r)
		ch <- result{data, err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("read after %s: %v", why, res.err)
		}
		return res.data
	case <-time.After(5 * time.Second):
		t.Fatalf("no EOF on the vsock side after %s: the host proxy would keep reusing a dead tunnel", why)
		return nil
	}
}

// A guest service that finishes and closes (Puma ending an idle keep-alive)
// must surface on the vsock as EOF, after any bytes it wrote. Waiting for both
// copy directions cannot do that: the vsock->target copy is parked in Read on
// an fd nobody will write to again.
func TestBridgePortForwardSurfacesTargetCloseOnVsock(t *testing.T) {
	f := newBridgeFixture(t)

	const response = "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"
	if _, err := f.backend.Write([]byte(response)); err != nil {
		t.Fatalf("backend write: %v", err)
	}
	f.backend.Close()

	got := readAllWithin(t, f.host, "the target closed")
	if string(got) != response {
		t.Fatalf("host read %q, want %q", got, response)
	}
	f.waitReturned(t, "the target closed")
}

// When the host proxy closes the vsock (the browser went away), the bridge must
// release the guest service connection rather than hold it until the service's
// own keep-alive timeout.
func TestBridgePortForwardReleasesTargetWhenVsockCloses(t *testing.T) {
	f := newBridgeFixture(t)

	if _, err := f.host.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatalf("host write: %v", err)
	}
	f.host.Close()

	readAllWithin(t, f.backend, "the vsock closed")
	f.waitReturned(t, "the vsock closed")
}
