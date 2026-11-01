package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync/atomic"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"

	"awgpilot/internal/netx"
)

// Ports used by tunStack connections. Replies to them are taken out of the tunnel before they
// reach Android; the system never uses ports this low for outgoing connections.
const stackPortLo, stackPortCount = 7300, 100

// tunStack is a small user-space TCP/IP stack spliced into the tunnel, so the app (which is
// excluded from its own VPN) can open TCP connections through the tunnel for the speed test.
// It uses the tunnel's own address; its packets are injected into the device next to the
// ping probes, and the replies are recognised by port.
type tunStack struct {
	s      *stack.Stack
	ep     *channel.Endpoint
	local  tcpip.Address
	port   atomic.Uint32
	cancel context.CancelFunc
}

func newTunStack(local netip.Addr, mtu int, out func([]byte) bool) (*tunStack, error) {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
		HandleLocal:        true,
	})
	sack := tcpip.TCPSACKEnabled(true)
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &sack)
	moderate := tcpip.TCPModerateReceiveBufferOption(true)
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &moderate)
	rcv := tcpip.TCPReceiveBufferSizeRangeOption{Min: 4 << 10, Default: 1 << 20, Max: 8 << 20}
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &rcv)
	snd := tcpip.TCPSendBufferSizeRangeOption{Min: 4 << 10, Default: 1 << 20, Max: 8 << 20}
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &snd)

	ep := channel.New(512, uint32(mtu), "")
	if err := s.CreateNIC(1, ep); err != nil {
		return nil, fmt.Errorf("CreateNIC: %v", err)
	}
	addr := tcpip.AddrFrom4(local.As4())
	if err := s.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: addr.WithPrefix()}, stack.AddressProperties{}); err != nil {
		return nil, fmt.Errorf("AddProtocolAddress: %v", err)
	}
	s.AddRoute(tcpip.Route{Destination: header.IPv4EmptySubnet, NIC: 1})

	ctx, cancel := context.WithCancel(context.Background())
	st := &tunStack{s: s, ep: ep, local: addr, cancel: cancel}
	go func() { // outbound packets from the stack go into the tunnel
		for {
			pkt := ep.ReadContext(ctx)
			if pkt.IsNil() {
				return
			}
			v := pkt.ToView()
			pkt.DecRef()
			b := append([]byte(nil), v.AsSlice()...)
			v.Release()
			if !out(b) {
				return
			}
		}
	}()
	return st, nil
}

// owns reports whether a packet coming out of the tunnel belongs to this stack.
func (st *tunStack) owns(b []byte) bool {
	if len(b) < 24 || b[0]>>4 != 4 || b[9] != 6 /* TCP */ {
		return false
	}
	ihl := int(b[0]&0x0f) * 4
	if len(b) < ihl+4 {
		return false
	}
	port := binary.BigEndian.Uint16(b[ihl+2:])
	return port >= stackPortLo && port < stackPortLo+stackPortCount
}

func (st *tunStack) inject(b []byte) {
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), b...))})
	st.ep.InjectInbound(ipv4.ProtocolNumber, pkt)
	pkt.DecRef()
}

// Dial opens a TCP connection through the tunnel; names are resolved by the system resolver.
func (st *tunStack) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		ips, lerr := netx.Resolver.LookupNetIP(ctx, "ip4", host)
		if lerr != nil || len(ips) == 0 {
			return nil, fmt.Errorf("не удалось найти адрес %s", host)
		}
		ip = ips[0].Unmap()
	}
	lport := stackPortLo + uint16(st.port.Add(1)%stackPortCount)
	return gonet.DialTCPWithBind(ctx, st.s,
		tcpip.FullAddress{NIC: 1, Addr: st.local, Port: lport},
		tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(ip.As4()), Port: uint16(port)},
		ipv4.ProtocolNumber)
}

func (st *tunStack) Close() {
	st.cancel()
	st.s.Close()
	st.ep.Close()
}
