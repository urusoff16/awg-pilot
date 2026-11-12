//go:build live

package xray

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

// pipeTun is a VPN adapter in memory: what the "system" stack sends comes out of Read, what the
// engine writes goes back into that stack.
type pipeTun struct {
	in     chan []byte
	system *channel.Endpoint
	events chan tun.Event
	closed chan struct{}
}

func (p *pipeTun) File() *os.File           { return nil }
func (p *pipeTun) MTU() (int, error)        { return 1500, nil }
func (p *pipeTun) Name() (string, error)    { return "pipe", nil }
func (p *pipeTun) Events() <-chan tun.Event { return p.events }
func (p *pipeTun) BatchSize() int           { return 1 }
func (p *pipeTun) Close() error {
	select {
	case <-p.closed:
	default:
		close(p.closed)
		close(p.events)
	}
	return nil
}

func (p *pipeTun) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	select {
	case b := <-p.in:
		sizes[0] = copy(bufs[0][offset:], b)
		return 1, nil
	case <-p.closed:
		return 0, os.ErrClosed
	}
}

func (p *pipeTun) Write(bufs [][]byte, offset int) (int, error) {
	for _, b := range bufs {
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), b[offset:]...))})
		p.system.InjectInbound(ipv4.ProtocolNumber, pkt)
		pkt.DecRef()
	}
	return len(bufs), nil
}

// TestLiveEngine carries real TCP and UDP through the first working server of $AWG_LINKS_FILE:
// a user-space "system" sends packets into the engine as Windows or Android would.
func TestLiveEngine(t *testing.T) {
	path := os.Getenv("AWG_LINKS_FILE")
	if path == "" {
		t.Skip("AWG_LINKS_FILE is not set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var link *Link
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.TrimSpace(l); l == "" {
			continue
		}
		if r := Test(context.Background(), l, 6*time.Second); r.Ms > 0 {
			link, _ = ParseLink(l)
			t.Logf("server %s (%s): %d ms", link.Name, link.Proto, r.Ms)
			break
		}
	}
	if link == nil {
		t.Fatal("no server answers")
	}
	addr, err := Resolve(context.Background(), link.Host)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := Start(link, addr)
	if err != nil {
		t.Fatal(err)
	}

	// the "system": a TCP/IP stack with the adapter's address, everything routed into the pipe
	sys := stack.New(stack.Options{NetworkProtocols: []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol}})
	ep := channel.New(256, 1500, "")
	if err := sys.CreateNIC(1, ep); err != nil {
		t.Fatal(err)
	}
	local := tcpip.AddrFrom4([4]byte{172, 19, 0, 1})
	sys.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: local.WithPrefix()}, stack.AddressProperties{})
	sys.AddRoute(tcpip.Route{Destination: header.IPv4EmptySubnet, NIC: 1})
	pt := &pipeTun{in: make(chan []byte, 256), system: ep, events: make(chan tun.Event, 1), closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			pkt := ep.ReadContext(ctx)
			if pkt == nil {
				return
			}
			v := pkt.ToView()
			pkt.DecRef()
			b := append([]byte(nil), v.AsSlice()...)
			v.Release()
			select {
			case pt.in <- b:
			case <-ctx.Done():
				return
			}
		}
	}()
	var taps int
	eng, err := NewEngine(inst, pt, EngineOptions{MTU: 1500, Tap: func([]byte, bool) { taps++ }})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	// TCP: Cloudflare's trace page shows the address the request came from: the server's exit
	dctx, dcancel := context.WithTimeout(ctx, 15*time.Second)
	c, err := gonet.DialContextTCP(dctx, sys, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{1, 1, 1, 1}), Port: 80}, ipv4.ProtocolNumber)
	dcancel()
	if err != nil {
		t.Fatalf("tcp: %v", err)
	}
	c.SetDeadline(time.Now().Add(15 * time.Second))
	req, _ := http.NewRequest(http.MethodGet, "http://1.1.1.1/cdn-cgi/trace", nil)
	req.Host = "one.one.one.one"
	req.Close = true
	if err := req.Write(c); err != nil {
		t.Fatalf("tcp write: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), req)
	if err != nil {
		t.Fatalf("tcp read: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	c.Close()
	exit := ""
	for _, l := range strings.Split(string(body), "\n") {
		if v, ok := strings.CutPrefix(l, "loc="); ok {
			exit = v
		}
	}
	if resp.StatusCode == 0 || !strings.Contains(strings.ToLower(string(body)+resp.Header.Get("Server")), "cloudflare") {
		t.Fatalf("tcp: %s %q", resp.Status, body)
	}
	t.Logf("TCP through the server: HTTP %d, exit country %s", resp.StatusCode, exit)

	// UDP: a DNS query to 1.1.1.1 the way the system resolver sends it
	u, err := gonet.DialUDP(sys, &tcpip.FullAddress{NIC: 1, Addr: local, Port: 40000}, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{1, 1, 1, 1}), Port: 53}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatalf("udp: %v", err)
	}
	defer u.Close()
	q := dnsQuery(0x1234, "example.com")
	var answer []byte
	for try := 0; try < 3 && answer == nil; try++ {
		u.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := u.Write(q); err != nil {
			t.Fatalf("udp write: %v", err)
		}
		buf := make([]byte, 1500)
		if n, err := u.Read(buf); err == nil {
			answer = buf[:n]
		}
	}
	if len(answer) < 12 || binary.BigEndian.Uint16(answer) != 0x1234 || binary.BigEndian.Uint16(answer[6:]) == 0 {
		t.Fatalf("udp: no DNS answer (%d bytes)", len(answer))
	}
	t.Logf("UDP through the server: DNS answer with %d records", binary.BigEndian.Uint16(answer[6:]))

	rx, tx, _ := eng.Counters()
	if rx == 0 || tx == 0 || taps == 0 {
		t.Fatalf("counters rx=%d tx=%d taps=%d", rx, tx, taps)
	}
	t.Logf("engine counted ↓ %d B, ↑ %d B, %d packets seen", rx, tx, taps)
	if _, err := net.ResolveIPAddr("ip", "localhost"); err != nil {
		t.Log(err)
	}
}

func dnsQuery(id uint16, name string) []byte {
	b := []byte{byte(id >> 8), byte(id), 1, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, part := range strings.Split(name, ".") {
		b = append(b, byte(len(part)))
		b = append(b, part...)
	}
	return append(b, 0, 0, 1, 0, 1)
}
