package sandbox

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// halfCloselessConn models the macOS vsock transport: VirtioSocketConnection
// exposes no CloseWrite, so a read blocks until the connection is closed
// outright. A guest that is holding an HTTP keep-alive open looks exactly like
// this from the host side.
type halfCloselessConn struct {
	closed chan struct{}
	once   sync.Once
}

func newHalfCloselessConn() *halfCloselessConn {
	return &halfCloselessConn{closed: make(chan struct{})}
}

func (c *halfCloselessConn) Read(b []byte) (int, error) {
	<-c.closed
	return 0, io.EOF
}

func (c *halfCloselessConn) Write(b []byte) (int, error) { return len(b), nil }

func (c *halfCloselessConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (c *halfCloselessConn) LocalAddr() net.Addr              { return fakeAddr{} }
func (c *halfCloselessConn) RemoteAddr() net.Addr             { return fakeAddr{} }
func (c *halfCloselessConn) SetDeadline(time.Time) error      { return nil }
func (c *halfCloselessConn) SetReadDeadline(time.Time) error  { return nil }
func (c *halfCloselessConn) SetWriteDeadline(time.Time) error { return nil }

type fakeAddr struct{}

func (fakeAddr) Network() string { return "vsock" }
func (fakeAddr) String() string  { return "vsock:0" }

// tcpPair returns the two ends of a real TCP connection, so closing one end
// produces a genuine EOF on the other (net.Pipe is synchronous and would not).
func tcpPair(t *testing.T) (client net.Conn, server net.Conn) {
	t.Helper()

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

	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	server = <-accepted
	if server == nil {
		t.Fatal("accept failed")
	}
	return client, server
}

// A client that goes away must release both sides of the proxy. Waiting for
// both copy directions cannot do that here: the guest->client copy is parked in
// Read until something closes the guest connection, and the only thing that
// would is the deferred Close that the wait itself is blocking.
func TestProxyConnReleasesBothSidesWhenClientDisconnects(t *testing.T) {
	guest := newHalfCloselessConn()
	m := &PortForwardManager{
		dialGuest: func(int) (net.Conn, error) { return guest, nil },
	}

	client, server := tcpPair(t)

	returned := make(chan struct{})
	go func() {
		m.proxyConn(context.Background(), server, 3000)
		close(returned)
	}()

	if _, err := client.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	client.Close()

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("proxyConn never returned after the client disconnected: " +
			"the host socket and its vsock connection are leaked for the life of the VM")
	}

	select {
	case <-guest.closed:
	default:
		t.Fatal("proxyConn returned without closing the guest connection")
	}
}
