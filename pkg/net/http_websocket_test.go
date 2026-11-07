package net

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/policy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func upstreamPort(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	require.NoError(t, err)
	return n
}

// startWebSocketEchoServer accepts one connection, answers the upgrade with a
// 101, then echoes every byte it receives. Frames are opaque to the proxy, so
// plain bytes stand in for them.
func startWebSocketEchoServer(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		req, err := http.ReadRequest(r)
		if err != nil {
			return
		}
		if !strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
			io.WriteString(conn, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
			return
		}
		io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: x\r\n\r\n")
		io.Copy(conn, r)
	}()
	return ln
}

func TestHandleHTTP_WebSocketUpgradeTunnelsBothWays(t *testing.T) {
	upstream := startWebSocketEchoServer(t)
	defer upstream.Close()
	_, portStr, _ := net.SplitHostPort(upstream.Addr().String())

	interceptor := NewHTTPInterceptor(policy.NewEngine(&api.NetworkConfig{
		AllowedHosts: []string{"127.0.0.1"},
	}), make(chan api.Event, 10), nil)

	client, server := net.Pipe()
	defer client.Close()
	go interceptor.HandleHTTP(server, "127.0.0.1", upstreamPort(t, portStr))

	client.SetDeadline(time.Now().Add(3 * time.Second))
	_, err := io.WriteString(client, "GET /socket HTTP/1.1\r\nHost: 127.0.0.1\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: x\r\nSec-WebSocket-Version: 13\r\n\r\n")
	require.NoError(t, err)

	reader := bufio.NewReader(client)
	resp, err := http.ReadResponse(reader, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

	for _, msg := range []string{"ping-frame", "second frame with no newline"} {
		_, err = io.WriteString(client, msg)
		require.NoError(t, err)
		buf := make([]byte, len(msg))
		_, err = io.ReadFull(reader, buf)
		require.NoError(t, err, "bytes sent after the 101 must come back through the proxy")
		assert.Equal(t, msg, string(buf))
	}
}

func TestHandleHTTP_NonUpgradeResponseKeepsRequestLoop(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		for {
			if _, err := http.ReadRequest(r); err != nil {
				return
			}
			io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
		}
	}()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())

	interceptor := NewHTTPInterceptor(policy.NewEngine(&api.NetworkConfig{
		AllowedHosts: []string{"127.0.0.1"},
	}), nil, nil)

	client, server := net.Pipe()
	defer client.Close()
	go interceptor.HandleHTTP(server, "127.0.0.1", upstreamPort(t, portStr))

	client.SetDeadline(time.Now().Add(3 * time.Second))
	reader := bufio.NewReader(client)
	for i := 0; i < 2; i++ {
		_, err := io.WriteString(client, "GET / HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n")
		require.NoError(t, err)
		resp, err := http.ReadResponse(reader, nil)
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		assert.Equal(t, "ok", string(body))
	}
}

func TestIsWebSocketUpgrade(t *testing.T) {
	req := &http.Request{Header: http.Header{"Connection": {"keep-alive, Upgrade"}, "Upgrade": {"WebSocket"}}}
	resp := &http.Response{StatusCode: 101, Header: http.Header{"Upgrade": {"websocket"}}}
	assert.True(t, isWebSocketUpgrade(req, resp))

	assert.False(t, isWebSocketUpgrade(req, &http.Response{StatusCode: 200, Header: resp.Header}))
	assert.False(t, isWebSocketUpgrade(req, &http.Response{StatusCode: 101, Header: http.Header{"Upgrade": {"h2c"}}}))
	assert.False(t, isWebSocketUpgrade(&http.Request{Header: http.Header{}}, resp))
}
