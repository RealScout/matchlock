//go:build darwin

package net

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/policy"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	// tcpReceiveWindowSize is the receive window advertised to the guest.
	// Larger windows improve throughput on high-bandwidth paths.
	tcpReceiveWindowSize = 4 << 20 // 4MB

	// socketBufSize is the SO_SNDBUF/SO_RCVBUF size for the socket pair.
	// Larger buffers prevent frame drops under burst traffic between VM and host.
	socketBufSize = 2 << 20 // 2MB

	// writeBufSize is the capacity of pooled write buffers for outbound packets.
	writeBufSize = 64 * 1024

	// dnsUpstreamTimeout bounds upstream DNS exchanges. Without a deadline,
	// transient host outbound-UDP failures wedge handleDNS goroutines forever
	// and leak the upstream UDP socket FD per query, eventually exhausting
	// the matchlock process FD limit and silently breaking DNS for every VM.
	dnsUpstreamTimeout = 5 * time.Second
)

type NetworkStack struct {
	stack       *stack.Stack
	policy      *policy.Engine
	interceptor *HTTPInterceptor
	events      chan api.Event
	linkEP      *socketPairEndpoint
	dnsServers  []string
	dnsIndex    atomic.Uint64
	gatewayIP   string
	mu          sync.Mutex
	closed      bool
}

type Config struct {
	FD         int
	File       *os.File // Use this instead of FD when available
	GatewayIP  string
	GuestIP    string
	MTU        uint32
	Policy     *policy.Engine
	Events     chan api.Event
	CAPool     *CAPool
	DNSServers []string
}

// writeBufPool provides reusable buffers for serializing outbound packets
// in WritePackets, which can be called from multiple goroutines concurrently.
var writeBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, writeBufSize)
		return &b
	},
}

// socketPairEndpoint implements stack.LinkEndpoint for Unix socket pairs
type socketPairEndpoint struct {
	file     *os.File
	mtu      uint32
	linkAddr tcpip.LinkAddress
	// dispatcher is read atomically in the hot path; only written on Attach.
	dispatcher    atomic.Pointer[stack.NetworkDispatcher]
	closed        atomic.Bool
	closeCh       chan struct{}
	mu            sync.Mutex // protects onCloseAction and linkAddr
	onCloseAction func()
}

func newSocketPairEndpoint(file *os.File, mtu uint32) *socketPairEndpoint {
	mac := tcpip.LinkAddress([]byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x01})
	return &socketPairEndpoint{
		file:     file,
		mtu:      mtu,
		linkAddr: mac,
		closeCh:  make(chan struct{}),
	}
}

func (e *socketPairEndpoint) MTU() uint32 {
	return e.mtu
}

func (e *socketPairEndpoint) SetMTU(mtu uint32) {
	e.mtu = mtu
}

func (e *socketPairEndpoint) MaxHeaderLength() uint16 {
	return header.EthernetMinimumSize
}

func (e *socketPairEndpoint) LinkAddress() tcpip.LinkAddress {
	return e.linkAddr
}

func (e *socketPairEndpoint) Capabilities() stack.LinkEndpointCapabilities {
	return stack.CapabilityResolutionRequired
}

func (e *socketPairEndpoint) Attach(dispatcher stack.NetworkDispatcher) {
	e.dispatcher.Store(&dispatcher)

	if dispatcher != nil {
		go e.readLoop()
	}
}

func (e *socketPairEndpoint) IsAttached() bool {
	d := e.dispatcher.Load()
	return d != nil && *d != nil
}

func (e *socketPairEndpoint) Wait() {}

func (e *socketPairEndpoint) SetLinkAddress(addr tcpip.LinkAddress) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.linkAddr = addr
}

func (e *socketPairEndpoint) ARPHardwareType() header.ARPHardwareType {
	return header.ARPHardwareEther
}

func (e *socketPairEndpoint) AddHeader(pkt *stack.PacketBuffer) {
	eth := header.Ethernet(pkt.LinkHeader().Push(header.EthernetMinimumSize))
	eth.Encode(&header.EthernetFields{
		SrcAddr: pkt.EgressRoute.LocalLinkAddress,
		DstAddr: pkt.EgressRoute.RemoteLinkAddress,
		Type:    pkt.NetworkProtocolNumber,
	})
}

func (e *socketPairEndpoint) ParseHeader(pkt *stack.PacketBuffer) bool {
	_, ok := pkt.LinkHeader().Consume(header.EthernetMinimumSize)
	return ok
}

func (e *socketPairEndpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	if e.closed.Load() {
		return 0, &tcpip.ErrClosedForSend{}
	}

	// Grab a write buffer from the pool; safe for concurrent callers.
	bp := writeBufPool.Get().(*[]byte)
	wb := *bp

	var written int
	for _, pkt := range pkts.AsSlice() {
		views := pkt.AsSlices()
		wb = wb[:0]
		for _, v := range views {
			wb = append(wb, v...)
		}
		if _, err := e.file.Write(wb); err != nil {
			*bp = wb
			writeBufPool.Put(bp)
			return written, &tcpip.ErrAborted{}
		}
		written++
	}

	*bp = wb
	writeBufPool.Put(bp)
	return written, nil
}

func (e *socketPairEndpoint) readLoop() {
	buf := make([]byte, e.mtu+header.EthernetMinimumSize)
	for {
		select {
		case <-e.closeCh:
			return
		default:
		}

		n, err := e.file.Read(buf)
		if err != nil {
			if e.closed.Load() {
				return
			}
			continue
		}

		if n < header.EthernetMinimumSize {
			continue
		}

		eth := header.Ethernet(buf[:n])
		proto := eth.Type()

		// We must allocate per packet because gVisor's TCP stack may hold
		// the underlying buffer.Buffer chunk in its receive/reassembly queue
		// long after the PacketBuffer is released. Pooling the byte slice via
		// OnRelease would cause use-after-free corruption under sustained load.
		data := make([]byte, n)
		copy(data, buf[:n])

		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: buffer.MakeWithData(data),
		})

		if !e.ParseHeader(pkt) {
			pkt.DecRef()
			continue
		}

		dp := e.dispatcher.Load()
		if dp != nil && *dp != nil {
			(*dp).DeliverNetworkPacket(proto, pkt)
		}
		pkt.DecRef()
	}
}

func (e *socketPairEndpoint) SetOnCloseAction(action func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onCloseAction = action
}

func (e *socketPairEndpoint) Close() {
	if !e.closed.CompareAndSwap(false, true) {
		return
	}

	e.mu.Lock()
	onClose := e.onCloseAction
	e.mu.Unlock()

	if onClose != nil {
		onClose()
	}
	close(e.closeCh)
	e.file.Close()
}

func NewNetworkStack(cfg *Config) (*NetworkStack, error) {
	file := cfg.File
	if file == nil {
		file = os.NewFile(uintptr(cfg.FD), "network")
	}

	// Increase socket pair buffer sizes to prevent frame drops under burst.
	setSocketBufferSizes(file)

	ns := &NetworkStack{
		policy:     cfg.Policy,
		events:     cfg.Events,
		dnsServers: cfg.DNSServers,
		gatewayIP:  cfg.GatewayIP,
	}
	ns.interceptor = NewHTTPInterceptor(cfg.Policy, cfg.Events, cfg.CAPool)

	s, linkEP, err := ns.buildStack(file, cfg.MTU)
	if err != nil {
		return nil, err
	}
	ns.stack = s
	ns.linkEP = linkEP

	return ns, nil
}

// buildStack constructs a fresh gVisor stack attached to the given socketpair
// FD. It is shared between NewNetworkStack and Reset so the stack-construction
// path stays in one place.
func (ns *NetworkStack) buildStack(file *os.File, mtu uint32) (*stack.Stack, *socketPairEndpoint, error) {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})

	// Tune TCP buffer sizes: raise max to allow auto-tuning up to 16MB
	// for better throughput on high-bandwidth paths.
	tcpSendBuf := tcpip.TCPSendBufferSizeRangeOption{
		Min:     tcp.MinBufferSize,         // 4KB
		Default: tcp.DefaultSendBufferSize, // 1MB
		Max:     16 << 20,                  // 16MB
	}
	tcpRecvBuf := tcpip.TCPReceiveBufferSizeRangeOption{
		Min:     tcp.MinBufferSize,            // 4KB
		Default: tcp.DefaultReceiveBufferSize, // 1MB
		Max:     16 << 20,                     // 16MB
	}
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &tcpSendBuf)
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &tcpRecvBuf)

	linkEP := newSocketPairEndpoint(file, mtu)

	if tcpipErr := s.CreateNIC(1, linkEP); tcpipErr != nil {
		s.Close()
		return nil, nil, fmt.Errorf("failed to create NIC: %v", tcpipErr)
	}

	gatewayAddr := tcpip.AddrFromSlice(net.ParseIP(ns.gatewayIP).To4())
	protoAddr := tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: gatewayAddr.WithPrefix(),
	}
	if tcpipErr := s.AddProtocolAddress(1, protoAddr, stack.AddressProperties{}); tcpipErr != nil {
		s.Close()
		return nil, nil, fmt.Errorf("failed to add address: %v", tcpipErr)
	}

	s.SetRouteTable([]tcpip.Route{{
		Destination: header.IPv4EmptySubnet,
		NIC:         1,
	}})

	s.SetPromiscuousMode(1, true)
	s.SetSpoofing(1, true)

	tcpForwarder := tcp.NewForwarder(s, tcpReceiveWindowSize, 65535, ns.handleTCPConnection)
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpForwarder.HandlePacket)

	udpForwarder := udp.NewForwarder(s, ns.handleUDPPacket)
	s.SetTransportProtocolHandler(udp.ProtocolNumber, udpForwarder.HandlePacket)

	return s, linkEP, nil
}

// Reset rebuilds the gVisor stack while keeping the underlying AF_UNIX
// socketpair alive. The guest sees no NIC bounce: same MAC, same routes,
// same kernel socket. In-flight TCP connections die (their gVisor endpoints
// close); apps retry and recover. Wedged DNS forwarder goroutines unblock
// when their endpoints close, releasing leaked upstream UDP socket FDs.
//
// This is the recovery path for VMs whose handleDNS goroutines wedged before
// the read-deadline fix landed and exhausted the matchlock-process FD limit.
func (ns *NetworkStack) Reset() error {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	if ns.closed {
		return ErrNetworkClosed
	}

	oldLinkEP := ns.linkEP
	oldStack := ns.stack
	mtu := oldLinkEP.mtu

	// Dup before close: a duplicate FD shares the same kernel-level open file
	// description, so closing the original leaves the socket alive for the
	// guest. Without this the VM's NIC would link-bounce on every reset.
	rawFD, err := syscall.Dup(int(oldLinkEP.file.Fd()))
	if err != nil {
		return fmt.Errorf("dup network FD: %w", err)
	}
	newFile := os.NewFile(uintptr(rawFD), "network")

	// Close the old gVisor stack first. This detaches the link endpoint via
	// Attach(nil), closes UDP/TCP endpoints, and unblocks any wedged DNS
	// forwarder goroutines waiting on guestConn reads.
	oldStack.Close()

	// Now close the old link endpoint. This closes its os.File FD, which
	// makes the in-flight Read in its readLoop return EBADF; the goroutine
	// then sees closed==true and exits cleanly. The underlying socket stays
	// alive because newFile still holds a reference to it.
	oldLinkEP.Close()

	s, linkEP, err := ns.buildStack(newFile, mtu)
	if err != nil {
		newFile.Close()
		return fmt.Errorf("%w: %v", ErrNetworkRebuild, err)
	}
	ns.stack = s
	ns.linkEP = linkEP
	return nil
}

func (ns *NetworkStack) handleTCPConnection(r *tcp.ForwarderRequest) {
	id := r.ID()
	dstPort := id.LocalPort

	var wq waiter.Queue
	ep, tcpipErr := r.CreateEndpoint(&wq)
	if tcpipErr != nil {
		r.Complete(true)
		return
	}

	r.Complete(false)
	guestConn := gonet.NewTCPConn(&wq, ep)

	dstIP := id.LocalAddress.String()

	switch dstPort {
	case 80:
		go ns.interceptor.HandleHTTP(guestConn, dstIP, int(dstPort))
	case 443:
		go ns.interceptor.HandleHTTPS(guestConn, dstIP, int(dstPort))
	default:
		host := fmt.Sprintf("%s:%d", dstIP, dstPort)
		if !ns.policy.IsHostAllowed(host) {
			ns.emitBlockedEvent(host, "host not in allowlist")
			guestConn.Close()
			return
		}
		go ns.handlePassthrough(guestConn, dstIP, int(dstPort))
	}
}

func (ns *NetworkStack) handlePassthrough(guestConn net.Conn, dstIP string, dstPort int) {
	defer guestConn.Close()

	if !ns.policy.IsHostAllowed(dstIP) {
		ns.emitBlockedEvent(net.JoinHostPort(dstIP, fmt.Sprintf("%d", dstPort)), "host not in allowlist")
		return
	}

	realConn, err := net.Dial("tcp", net.JoinHostPort(dstIP, fmt.Sprintf("%d", dstPort)))
	if err != nil {
		return
	}
	defer realConn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		copyWithCancel(ctx, realConn, guestConn)
		cancel()
	}()
	go func() {
		copyWithCancel(ctx, guestConn, realConn)
		cancel()
	}()

	<-ctx.Done()
}

func copyWithCancel(ctx context.Context, dst, src net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := src.Read(buf)
		if n > 0 {
			dst.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func (ns *NetworkStack) handleUDPPacket(r *udp.ForwarderRequest) bool {
	id := r.ID()

	if id.LocalPort == 53 {
		ns.handleDNS(r)
		return true
	}

	// Non-DNS UDP: silently drop by not creating an endpoint.
	return true
}

func (ns *NetworkStack) handleDNS(r *udp.ForwarderRequest) {
	var wq waiter.Queue
	ep, tcpipErr := r.CreateEndpoint(&wq)
	if tcpipErr != nil {
		return
	}

	guestConn := gonet.NewUDPConn(&wq, ep)
	defer guestConn.Close()

	buf := make([]byte, 512)
	n, _, err := guestConn.ReadFrom(buf)
	if err != nil {
		return
	}

	if len(ns.dnsServers) == 0 {
		slog.Debug("dropping DNS query; no upstream servers configured")
		return
	}
	idx := ns.dnsIndex.Add(1) - 1
	server := ns.dnsServers[idx%uint64(len(ns.dnsServers))]
	resp, err := exchangeDNS(buf[:n], server, dnsUpstreamTimeout)
	if err != nil {
		slog.Debug("DNS upstream exchange failed", "server", server, "error", err)
		return
	}

	guestConn.Write(resp)
}

func exchangeDNS(query []byte, server string, timeout time.Duration) ([]byte, error) {
	dnsConn, err := net.DialTimeout("udp", dnsServerAddr(server), timeout)
	if err != nil {
		return nil, err
	}
	defer dnsConn.Close()

	// Bound write+read so a transient host-side UDP disruption can't pin this
	// goroutine and its FD forever. Without this the daemon leaks one UDP
	// socket per stuck query and eventually wedges DNS for every VM.
	if err := dnsConn.SetDeadline(time.Now().Add(timeout)); err != nil {
		slog.Warn("set DNS deadline failed", "server", server, "error", err)
		return nil, err
	}

	if _, err := dnsConn.Write(query); err != nil {
		return nil, err
	}

	resp := make([]byte, 512)
	respN, err := dnsConn.Read(resp)
	if err != nil {
		return nil, err
	}

	return resp[:respN], nil
}

func dnsServerAddr(server string) string {
	if _, _, err := net.SplitHostPort(server); err == nil {
		return server
	}
	return net.JoinHostPort(server, "53")
}

func (ns *NetworkStack) emitBlockedEvent(host, reason string) {
	if ns.events != nil {
		select {
		case ns.events <- api.Event{
			Type: "network",
			Network: &api.NetworkEvent{
				Host:        host,
				Blocked:     true,
				BlockReason: reason,
			},
		}:
		default:
		}
	}
}

func (ns *NetworkStack) Close() error {
	ns.mu.Lock()
	defer ns.mu.Unlock()

	if ns.closed {
		return nil
	}
	ns.closed = true

	ns.linkEP.Close()
	ns.stack.Close()
	return nil
}

func (ns *NetworkStack) Stack() *stack.Stack {
	return ns.stack
}
