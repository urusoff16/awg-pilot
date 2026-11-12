package main

import (
	"context"
	"net"
	"sync/atomic"
	"time"

	"awgpilot/internal/xray"
)

// vpnTunnel is a running tunnel: AmneziaWG (tunnel) or a subscription server (proxyTunnel).
type vpnTunnel interface {
	Close()
	// Rebind follows a switch of the phone's network.
	Rebind()
	Stats() Stats
	// speedDialer opens TCP connections through the tunnel for the speed test; done ends them.
	speedDialer() (dial func(ctx context.Context, network, addr string) (net.Conn, error), done func(), err error)
}

func (t *tunnel) speedDialer() (func(ctx context.Context, network, addr string) (net.Conn, error), func(), error) {
	st, err := t.pt.startStack(t.mtu)
	if err != nil {
		return nil, nil, err
	}
	return st.Dial, func() { t.pt.stopStack(st) }, nil
}

// proxyTunnel carries the VPN interface through a subscription server with Xray. The app itself
// is outside its VPN, so Xray's connections to the server go straight over the phone's network.
type proxyTunnel struct {
	eng       *xray.Engine
	since     time.Time
	endpoint  string
	name      string
	serverID  string
	v6Blocked bool
	rtt       atomic.Int64 // last request through the server, ms; -1 = no answer
	fails     atomic.Int32
	lastOK    atomic.Int64 // unix nanoseconds of the last answer through the server
	stop      chan struct{}
}

// proxyProbeEvery is how often the server is checked while connected (the ping of AmneziaWG).
const proxyProbeEvery = 5 * time.Second

// startProxy takes ownership of fd (closed on failure).
func startProxy(fd int, link *xray.Link, serverID, addr string, mtu int, dropV6 bool, tap packetTap) (*proxyTunnel, error) {
	td, err := tunFromFD(fd)
	if err != nil {
		return nil, err
	}
	inst, err := xray.Start(link, addr)
	if err != nil {
		td.Close()
		return nil, err
	}
	opt := xray.EngineOptions{MTU: mtu, DropIPv6: dropV6}
	if tap != nil {
		opt.Tap = tap.Packet
	}
	eng, err := xray.NewEngine(inst, td, opt)
	if err != nil {
		inst.Close()
		td.Close()
		return nil, err
	}
	t := &proxyTunnel{eng: eng, since: time.Now(), endpoint: link.Endpoint(), name: link.Name, serverID: serverID,
		v6Blocked: dropV6, stop: make(chan struct{})}
	t.rtt.Store(-1)
	go t.probeLoop()
	return t, nil
}

func (t *proxyTunnel) probeLoop() {
	tick := time.NewTicker(proxyProbeEvery)
	defer tick.Stop()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		d, err := t.eng.Instance().Delay(ctx)
		cancel()
		select {
		case <-t.stop:
			return
		default:
		}
		if err == nil {
			t.rtt.Store(max(d.Milliseconds(), 1))
			t.fails.Store(0)
			t.lastOK.Store(time.Now().UnixNano())
		} else {
			t.rtt.Store(-1)
			t.fails.Add(1)
		}
		select {
		case <-t.stop:
			return
		case <-tick.C:
		}
	}
}

func (t *proxyTunnel) Close() {
	close(t.stop)
	t.eng.Close()
}

func (t *proxyTunnel) Rebind() {} // new connections take the new network by themselves

func (t *proxyTunnel) Stats() Stats {
	rx, tx, _ := t.eng.Counters()
	s := Stats{Up: true, Since: t.since, Endpoint: t.endpoint, ServerName: t.name, Proxy: true, IPv6Blocked: t.v6Blocked,
		RxBytes: rx, TxBytes: tx, PingMs: t.rtt.Load(), PingFails: int(t.fails.Load())}
	if ok := t.lastOK.Load(); ok != 0 {
		s.LastHandshake = time.Unix(0, ok)
	}
	return s
}

func (t *proxyTunnel) speedDialer() (func(ctx context.Context, network, addr string) (net.Conn, error), func(), error) {
	inst := t.eng.Instance()
	return func(ctx context.Context, _, addr string) (net.Conn, error) { return inst.Dial(ctx, addr) }, func() {}, nil
}
