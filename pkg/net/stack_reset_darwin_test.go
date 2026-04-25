//go:build darwin

package net

import (
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/policy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestNetworkStack(t *testing.T) (*NetworkStack, *os.File) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	require.NoError(t, err, "socketpair")
	require.NoError(t, syscall.SetNonblock(fds[0], true), "setnonblock host")
	require.NoError(t, syscall.SetNonblock(fds[1], true), "setnonblock peer")
	hostFile := os.NewFile(uintptr(fds[0]), "host")
	peerFile := os.NewFile(uintptr(fds[1]), "peer")

	ns, err := NewNetworkStack(&Config{
		File:       hostFile,
		GatewayIP:  "192.168.101.1",
		GuestIP:    "192.168.101.2",
		MTU:        1500,
		Policy:     policy.NewEngine(&api.NetworkConfig{}),
		Events:     make(chan api.Event, 16),
		DNSServers: []string{"8.8.8.8"},
	})
	if err != nil {
		hostFile.Close()
		peerFile.Close()
		require.NoError(t, err, "NewNetworkStack")
	}
	t.Cleanup(func() {
		_ = ns.Close()
		_ = peerFile.Close()
	})
	return ns, peerFile
}

func TestNetworkStackReset_Succeeds(t *testing.T) {
	ns, _ := newTestNetworkStack(t)

	oldStack := ns.stack
	oldLinkEP := ns.linkEP

	require.NoError(t, ns.Reset())

	assert.NotSame(t, oldStack, ns.stack, "stack should be replaced")
	assert.NotSame(t, oldLinkEP, ns.linkEP, "link endpoint should be replaced")
	assert.Equal(t, oldLinkEP.mtu, ns.linkEP.mtu, "MTU should be preserved")
}

func TestNetworkStackReset_PreservesGuestSocket(t *testing.T) {
	ns, peer := newTestNetworkStack(t)

	require.NoError(t, ns.Reset())

	// Send a frame from peer to host. It must reach the new socketPairEndpoint
	// via the dup'd FD. Without dup the underlying kernel socket would have been
	// torn down and this Write would return EPIPE.
	frame := []byte{
		0x02, 0, 0, 0, 0, 1, // dst MAC
		0x02, 0, 0, 0, 0, 2, // src MAC
		0x08, 0x00, // ethertype IPv4
		0xde, 0xad, 0xbe, 0xef, // payload
	}
	_, err := peer.Write(frame)
	require.NoError(t, err, "guest-side socket should survive reset")

	// Small grace period for the new readLoop to consume the frame before
	// the test teardown closes the stack.
	time.Sleep(50 * time.Millisecond)
}

func TestNetworkStackReset_AfterClose(t *testing.T) {
	ns, _ := newTestNetworkStack(t)

	require.NoError(t, ns.Close())
	err := ns.Reset()
	assert.ErrorIs(t, err, ErrNetworkClosed)
}

func TestNetworkStackReset_Idempotent(t *testing.T) {
	ns, _ := newTestNetworkStack(t)

	for i := 0; i < 3; i++ {
		require.NoErrorf(t, ns.Reset(), "Reset #%d", i)
	}
}
