package xray

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
	"github.com/xtls/xray-core/common/buf"
	xctx "github.com/xtls/xray-core/common/ctx"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/routing"
	xtun "github.com/xtls/xray-core/proxy/tun"
	"github.com/xtls/xray-core/transport"
)

// room is what the device may need in front of a packet (amneziawg-go's offset, Linux's
// virtio header); Xray's stack reserves the same.
const room = 16

// EngineOptions configure an Engine.
type EngineOptions struct {
	MTU int
	// DropIPv6 discards IPv6 from the system: the IPv6 default route only exists so that nothing
	// leaves beside the tunnel, and apps fall back to IPv4 inside it.
	DropIPv6 bool
	// Tap sees every packet: out=true from the system to the tunnel, false back.
	Tap func(pkt []byte, out bool)
	// Sniff reads the site name from each new connection (TLS, HTTP, QUIC) for routing by domain;
	// the connection still goes to the address the app asked for.
	Sniff bool
}

// Engine carries the packets of a VPN adapter through an Xray server: Xray's TCP/IP stack turns
// them into connections, and each connection goes to the server.
type Engine struct {
	inst  *Instance
	dev   tun.Device
	stack xtun.Stack
	ctx   context.Context
	stop  context.CancelFunc
	opt   EngineOptions

	dispatcher routing.Dispatcher

	rx, tx atomic.Uint64
	lastRx atomic.Int64 // unix nanoseconds of the last packet back from the server
	since  time.Time
	once   sync.Once

	wmu  sync.Mutex // amneziawg-go devices take writes from one goroutine at a time
	wbuf []byte

	// a device may hand over several packets at once; the stack takes them one by one
	rbufs  [][]byte
	sizes  []int
	queued int
	next   int
}

// NewEngine starts carrying dev's packets through inst. The engine owns dev and inst from now on.
func NewEngine(inst *Instance, dev tun.Device, opt EngineOptions) (*Engine, error) {
	if opt.MTU <= 0 {
		opt.MTU = 1500
	}
	d := inst.core.GetFeature(routing.DispatcherType())
	if d == nil {
		return nil, errors.New("в Xray нет маршрутизатора соединений")
	}
	ctx, stop := context.WithCancel(context.WithValue(context.Background(), core.XrayKey(1), inst.core))
	e := &Engine{inst: inst, dev: dev, ctx: ctx, stop: stop, opt: opt, dispatcher: d.(routing.Dispatcher),
		since: time.Now(), wbuf: make([]byte, room+1<<16)}
	n := dev.BatchSize()
	if n < 1 {
		n = 1
	}
	e.rbufs = make([][]byte, n)
	for i := range e.rbufs {
		e.rbufs[i] = make([]byte, room+1<<16)
	}
	e.sizes = make([]int, n)

	go func() { // nobody else reads the device's events; a full channel would stall its sender
		for range dev.Events() {
		}
	}()
	st, err := xtun.NewStack(ctx, xtun.StackOptions{Tun: (*engineTun)(e), MTU: uint32(opt.MTU), IdleTimeout: 5 * time.Minute}, e)
	if err != nil {
		stop()
		return nil, err
	}
	e.stack = st
	if err := st.Start(); err != nil {
		stop()
		return nil, err
	}
	return e, nil
}

// Close stops the stack, the device and the core.
func (e *Engine) Close() {
	e.once.Do(func() {
		e.stop()
		e.stack.Close()
		e.dev.Close()
		e.inst.Close()
	})
}

// Instance is the core the engine sends connections to.
func (e *Engine) Instance() *Instance { return e.inst }

// Counters returns the bytes received from and sent to the tunnel, and when something last came
// back from the server.
func (e *Engine) Counters() (rx, tx uint64, lastRx time.Time) {
	if t := e.lastRx.Load(); t != 0 {
		lastRx = time.Unix(0, t)
	}
	return e.rx.Load(), e.tx.Load(), lastRx
}

// HandleConnection passes a connection of the stack to the server (as Xray's TUN inbound does).
func (e *Engine) HandleConnection(conn xnet.Conn, dest xnet.Destination) {
	defer conn.Close()
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	ctx = xctx.ContextWithID(ctx, session.NewID())
	inbound := session.Inbound{
		Name:          "tun",
		Tag:           "tun",
		CanSpliceCopy: 3,
		Source:        xnet.DestinationFromAddr(conn.RemoteAddr()),
		User:          &protocol.MemoryUser{Level: 0},
	}
	ctx = session.ContextWithInbound(ctx, &inbound)
	content := &session.Content{}
	if e.opt.Sniff {
		content.SniffingRequest = session.SniffingRequest{Enabled: true, OverrideDestinationForProtocol: []string{"http", "tls", "quic"}, RouteOnly: true}
	}
	ctx = session.ContextWithContent(ctx, content)
	ctx = session.SubContextFromMuxInbound(ctx)

	var writer buf.Writer = buf.NewWriter(conn)
	if dest.Network != xnet.Network_UDP {
		writer = &xtun.TCPWriter{Writer: writer}
	}
	link := &transport.Link{Reader: &buf.TimeoutWrapperReader{Reader: buf.NewReader(conn)}, Writer: writer}
	e.dispatcher.DispatchLink(ctx, dest, link)
}

// engineTun is the device as Xray's stack sees it.
type engineTun Engine

func (t *engineTun) Start() error           { return nil }
func (t *engineTun) Close() error           { return nil } // Engine.Close closes the device
func (t *engineTun) Name() (string, error)  { return t.dev.Name() }
func (t *engineTun) Index() (int, error)    { return 0, nil }
func (t *engineTun) e() *Engine             { return (*Engine)(t) }
func isIPv6(p []byte) bool                  { return len(p) > 0 && p[0]>>4 == 6 }
func (t *engineTun) drop(p []byte) bool     { return t.opt.DropIPv6 && isIPv6(p) }
func (t *engineTun) count(p []byte, o bool) { t.e().countPacket(p, o) }

func (e *Engine) countPacket(p []byte, out bool) {
	if out {
		e.tx.Add(uint64(len(p)))
	} else {
		e.rx.Add(uint64(len(p)))
		e.lastRx.Store(time.Now().UnixNano())
	}
	if e.opt.Tap != nil {
		e.opt.Tap(p, out)
	}
}

// ReadPacket waits for the next packet from the system.
func (t *engineTun) ReadPacket(b []byte) ([]byte, error) {
	for {
		if t.next < t.queued {
			i := t.next
			t.next++
			p := t.rbufs[i][room : room+t.sizes[i]]
			if t.drop(p) {
				continue
			}
			t.count(p, true)
			n := copy(b[room:], p)
			return b[room : room+n], nil
		}
		n, err := t.dev.Read(t.rbufs, t.sizes, room)
		if err != nil {
			if errors.Is(err, tun.ErrTooManySegments) {
				continue
			}
			if errors.Is(err, os.ErrClosed) {
				return nil, err
			}
			if t.ctx.Err() != nil {
				return nil, t.ctx.Err()
			}
			return nil, err
		}
		t.queued, t.next = n, 0
	}
}

// WritePacket hands a packet from the server to the system.
func (t *engineTun) WritePacket(header, payload []byte) error {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	n := copy(t.wbuf[room:], header)
	n += copy(t.wbuf[room+n:], payload)
	p := t.wbuf[room : room+n]
	t.count(p, false)
	_, err := t.dev.Write([][]byte{t.wbuf[:room+n]}, room)
	return err
}
