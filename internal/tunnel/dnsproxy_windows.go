package tunnel

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/sys/windows"

	"github.com/amnezia-vpn/amneziawg-windows/tunnel/winipcfg"

	"awgpilot/internal/rudomains"
	"awgpilot/internal/xray"
)

// directDNS resolves the "Russia past the VPN" domains straight from Russia, so the answers point
// at Russian servers (Yandex DNS, over the physical network).
var directDNS = netip.MustParseAddr("77.88.8.8")

// maxDirectRoutes bounds the routes the proxy adds (a long session opens many sites).
const maxDirectRoutes = 3000

// route is a /32 the proxy added.
type route struct {
	luid winipcfg.LUID
	hop  net.IP
}

// dnsProxy is "Russia past the VPN" by domain for AmneziaWG: the system asks it for every name;
// a Russian domain is resolved straight and its addresses get /32 routes over the physical
// gateway before the answer goes back, everything else is asked through the tunnel. Routes appear
// only for the sites actually opened, instead of the twenty thousand of the networks list.
type dnsProxy struct {
	conn     *net.UDPConn
	upstream []netip.Addr
	match    *rudomains.Matcher
	ours     winipcfg.LUID
	logf     func(string, ...any)
	closed   atomic.Bool
	done     chan struct{}

	mu     sync.Mutex
	gwLUID winipcfg.LUID // physical interface and gateway the routes go through
	gwHop  net.IP
	gwIdx  uint32
	gwAt   time.Time
	routes map[netip.Addr]route
	warned bool
}

func startDNSProxy(listen netip.Addr, upstream []netip.Addr, domains []string, ours winipcfg.LUID, logf func(string, ...any)) (*dnsProxy, error) {
	// The tunnel's address becomes usable a moment after it is set (Windows checks it first).
	var c *net.UDPConn
	var err error
	for try := 0; try < 50; try++ {
		if c, err = net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(listen, 53))); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return nil, err
	}
	p := &dnsProxy{conn: c, upstream: upstream, match: rudomains.NewMatcher(domains), ours: ours, logf: logf,
		routes: map[netip.Addr]route{}, done: make(chan struct{})}
	go p.serve()
	go p.watchGateway()
	return p, nil
}

func (p *dnsProxy) serve() {
	buf := make([]byte, 4096)
	for {
		n, from, err := p.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if p.closed.Load() {
				return
			}
			continue
		}
		req := append([]byte(nil), buf[:n]...)
		go p.handle(req, from)
	}
}

func (p *dnsProxy) handle(req []byte, from netip.AddrPort) {
	var parser dnsmessage.Parser
	hdr, err := parser.Start(req)
	if err != nil {
		return
	}
	q, err := parser.Question()
	if err != nil {
		return
	}
	var resp []byte
	if p.match.Match(q.Name.String()) {
		if q.Type == dnsmessage.TypeAAAA {
			resp = emptyAnswer(hdr, q) // Russian sites over IPv4: their routes are IPv4 only
		} else if resp, err = p.exchange(directDNS, req, true); err == nil {
			p.routeAnswers(resp)
		}
	}
	if resp == nil {
		for _, up := range p.upstream {
			if resp, err = p.exchange(up, req, false); err == nil {
				break
			}
		}
	}
	if resp != nil {
		p.conn.WriteToUDPAddrPort(resp, from)
	}
}

// exchange asks server; physical pins the socket to the physical network (past the tunnel).
func (p *dnsProxy) exchange(server netip.Addr, req []byte, physical bool) ([]byte, error) {
	var lc net.ListenConfig
	if physical {
		_, _, idx := p.gateway()
		if idx == 0 {
			return nil, errors.New("нет физической сети")
		}
		b := &xray.Binder{}
		b.BindSocketToInterface4(idx, false)
		lc.Control = func(network, _ string, c syscall.RawConn) error {
			var herr error
			if err := c.Control(func(fd uintptr) { herr = b.Hook(network, fd) }); err != nil {
				return err
			}
			return herr
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	pc, err := lc.ListenPacket(ctx, "udp4", ":0")
	if err != nil {
		return nil, err
	}
	defer pc.Close()
	pc.SetDeadline(time.Now().Add(4 * time.Second))
	to := net.UDPAddrFromAddrPort(netip.AddrPortFrom(server, 53))
	if _, err := pc.WriteTo(req, to); err != nil {
		return nil, err
	}
	buf := make([]byte, 4096)
	for {
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			return nil, err
		}
		if n >= 2 && buf[0] == req[0] && buf[1] == req[1] {
			return append([]byte(nil), buf[:n]...), nil
		}
	}
}

// routeAnswers adds a route over the physical gateway for every IPv4 address in the answer.
func (p *dnsProxy) routeAnswers(resp []byte) {
	var parser dnsmessage.Parser
	if _, err := parser.Start(resp); err != nil {
		return
	}
	if err := parser.SkipAllQuestions(); err != nil {
		return
	}
	for {
		h, err := parser.AnswerHeader()
		if err != nil {
			return
		}
		if h.Type != dnsmessage.TypeA {
			if parser.SkipAnswer() != nil {
				return
			}
			continue
		}
		a, err := parser.AResource()
		if err != nil {
			return
		}
		p.addRoute(netip.AddrFrom4(a.A))
	}
}

func (p *dnsProxy) addRoute(ip netip.Addr) {
	luid, hop, _ := p.gateway()
	if luid == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed.Load() {
		return
	}
	if old, ok := p.routes[ip]; ok && old.luid == luid {
		return
	}
	if len(p.routes) >= maxDirectRoutes {
		if !p.warned {
			p.warned = true
			p.logf("Россия мимо VPN: достигнут предел %d маршрутов, новые российские адреса пойдут через VPN", maxDirectRoutes)
		}
		return
	}
	dst := net.IPNet{IP: net.IP(ip.AsSlice()), Mask: net.CIDRMask(32, 32)}
	if err := luid.AddRoute(dst, hop, 0); err != nil && !errors.Is(err, windows.ERROR_OBJECT_ALREADY_EXISTS) {
		return
	}
	p.routes[ip] = route{luid, hop}
}

// gateway is the physical interface with the best default route and its next hop (cached 5 s).
func (p *dnsProxy) gateway() (winipcfg.LUID, net.IP, uint32) {
	p.mu.Lock()
	if time.Since(p.gwAt) < 5*time.Second {
		defer p.mu.Unlock()
		return p.gwLUID, p.gwHop, p.gwIdx
	}
	p.mu.Unlock()
	luid, hop, idx := bestPhysical(p.ours)
	p.mu.Lock()
	p.gwLUID, p.gwHop, p.gwIdx, p.gwAt = luid, hop, idx, time.Now()
	p.mu.Unlock()
	return luid, hop, idx
}

func bestPhysical(ours winipcfg.LUID) (winipcfg.LUID, net.IP, uint32) {
	r, err := winipcfg.GetIPForwardTable2(windows.AF_INET)
	if err != nil {
		return 0, nil, 0
	}
	best, bestMetric := -1, ^uint32(0)
	for i := range r {
		if r[i].DestinationPrefix.PrefixLength != 0 || r[i].InterfaceLUID == ours {
			continue
		}
		ifrow, err := r[i].InterfaceLUID.Interface()
		if err != nil || ifrow.OperStatus != winipcfg.IfOperStatusUp || isVirtual(ifrow.Type) {
			continue
		}
		iface, err := r[i].InterfaceLUID.IPInterface(windows.AF_INET)
		if err != nil {
			continue
		}
		if m := r[i].Metric + iface.Metric; m < bestMetric {
			best, bestMetric = i, m
		}
	}
	if best < 0 {
		return 0, nil, 0
	}
	return r[best].InterfaceLUID, r[best].NextHop.IP(), r[best].InterfaceIndex
}

// watchGateway moves the routes when the physical network changes (Wi-Fi ↔ cable ↔ phone).
func (p *dnsProxy) watchGateway() {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-t.C:
		}
		luid, _, _ := p.gateway()
		if luid == 0 {
			continue
		}
		p.mu.Lock()
		var move []netip.Addr
		for ip, r := range p.routes {
			if r.luid != luid {
				move = append(move, ip)
				r.luid.DeleteRoute(net.IPNet{IP: net.IP(ip.AsSlice()), Mask: net.CIDRMask(32, 32)}, r.hop)
				delete(p.routes, ip)
			}
		}
		p.mu.Unlock()
		for _, ip := range move {
			p.addRoute(ip)
		}
	}
}

// Close stops the proxy and removes its routes.
func (p *dnsProxy) Close() {
	if !p.closed.CompareAndSwap(false, true) {
		return
	}
	close(p.done)
	p.conn.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	for ip, r := range p.routes {
		r.luid.DeleteRoute(net.IPNet{IP: net.IP(ip.AsSlice()), Mask: net.CIDRMask(32, 32)}, r.hop)
	}
	p.routes = nil
}

// emptyAnswer is a successful answer without records.
func emptyAnswer(h dnsmessage.Header, q dnsmessage.Question) []byte {
	h.Response, h.RecursionAvailable, h.RCode = true, true, dnsmessage.RCodeSuccess
	b := dnsmessage.NewBuilder(nil, h)
	b.StartQuestions()
	b.Question(q)
	out, _ := b.Finish()
	return out
}
