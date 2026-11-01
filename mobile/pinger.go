package main

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
)

// pingTun wraps the VPN interface and measures the round trip through the tunnel by injecting
// ICMP echo requests into the device's outbound stream and catching the replies before they
// reach Android. The app itself is excluded from its VPN, so it cannot ping through it with a
// socket; this also keeps the probes invisible to other apps.
type pingTun struct {
	tun.Device
	src, dst netip.Addr

	in     chan packet
	inject chan []byte
	done   chan struct{}
	once   sync.Once
	pool   sync.Pool

	id      uint16
	seq     uint16
	mu      sync.Mutex
	pending map[uint16]time.Time

	rtt   atomic.Int64 // last round trip in ms; -1 = no reply yet / lost
	fails atomic.Int32 // consecutive unanswered probes

	stk atomic.Pointer[tunStack] // speed test stack while a test runs
}

type packet struct {
	buf []byte // pooled, len = packet size
	err error
}

const maxPacket = 65535

func newPingTun(dev tun.Device, src, dst netip.Addr) *pingTun {
	p := &pingTun{Device: dev, src: src, dst: dst, in: make(chan packet, 64), inject: make(chan []byte, 256),
		done: make(chan struct{}), pending: map[uint16]time.Time{}}
	p.pool.New = func() any { b := make([]byte, maxPacket); return &b }
	var r [2]byte
	if f, err := os.Open("/dev/urandom"); err == nil {
		f.Read(r[:])
		f.Close()
	}
	p.id = binary.BigEndian.Uint16(r[:]) | 1
	p.rtt.Store(-1)
	go p.pump()
	if src.Is4() && dst.Is4() {
		go p.probeLoop()
	}
	return p
}

// pump reads packets from Android so Read can also hand out injected probes while idle.
func (p *pingTun) pump() {
	n := p.Device.BatchSize()
	bufs := make([][]byte, n)
	ptrs := make([]*[]byte, n)
	sizes := make([]int, n)
	for {
		for i := range bufs {
			ptrs[i] = p.pool.Get().(*[]byte)
			bufs[i] = *ptrs[i]
		}
		cnt, err := p.Device.Read(bufs, sizes, 0)
		for i := 0; i < n; i++ {
			if i < cnt && err == nil {
				select {
				case p.in <- packet{buf: (*ptrs[i])[:sizes[i]]}:
				case <-p.done:
					return
				}
			} else {
				p.pool.Put(ptrs[i])
			}
		}
		if err != nil {
			select {
			case p.in <- packet{err: err}:
			case <-p.done:
			}
			return
		}
	}
}

func (p *pingTun) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	select {
	case b := <-p.inject:
		sizes[0] = copy(bufs[0][offset:], b)
		return 1, nil
	case pk := <-p.in:
		if pk.err != nil {
			return 0, pk.err
		}
		sizes[0] = copy(bufs[0][offset:], pk.buf)
		b := pk.buf[:cap(pk.buf)]
		p.pool.Put(&b)
		return 1, nil
	case <-p.done:
		return 0, os.ErrClosed
	}
}

// Write delivers packets to Android, dropping the replies to our own probes.
func (p *pingTun) Write(bufs [][]byte, offset int) (int, error) {
	keep := bufs[:0:0]
	st := p.stk.Load()
	for _, b := range bufs {
		if p.isOurReply(b[offset:]) {
			continue
		}
		if st != nil && st.owns(b[offset:]) {
			st.inject(b[offset:])
			continue
		}
		keep = append(keep, b)
	}
	if len(keep) == 0 {
		return len(bufs), nil
	}
	if _, err := p.Device.Write(keep, offset); err != nil {
		return 0, err
	}
	return len(bufs), nil
}

func (p *pingTun) Close() error {
	p.once.Do(func() { close(p.done) })
	if st := p.stk.Swap(nil); st != nil {
		st.Close()
	}
	return p.Device.Close()
}

// startStack splices a TCP/IP stack into the tunnel for the speed test.
func (p *pingTun) startStack(mtu int) (*tunStack, error) {
	if !p.src.Is4() {
		return nil, errors.New("у туннеля нет IPv4-адреса")
	}
	st, err := newTunStack(p.src, mtu, func(b []byte) bool {
		select {
		case p.inject <- b:
			return true
		case <-p.done:
			return false
		}
	})
	if err != nil {
		return nil, err
	}
	if old := p.stk.Swap(st); old != nil {
		old.Close()
	}
	return st, nil
}

func (p *pingTun) stopStack(st *tunStack) {
	if p.stk.CompareAndSwap(st, nil) {
		st.Close()
	}
}

func (p *pingTun) isOurReply(b []byte) bool {
	if len(b) < 28 || b[0]>>4 != 4 || b[9] != 1 {
		return false
	}
	ihl := int(b[0]&0x0f) * 4
	if len(b) < ihl+8 || b[ihl] != 0 /* echo reply */ || binary.BigEndian.Uint16(b[ihl+4:]) != p.id {
		return false
	}
	seq := binary.BigEndian.Uint16(b[ihl+6:])
	p.mu.Lock()
	sent, ok := p.pending[seq]
	delete(p.pending, seq)
	p.mu.Unlock()
	if ok {
		p.rtt.Store(time.Since(sent).Milliseconds())
		p.fails.Store(0)
	}
	return true
}

func (p *pingTun) probeLoop() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-t.C:
		}
		p.mu.Lock()
		for seq, sent := range p.pending { // unanswered within the interval: lost
			if time.Since(sent) > 4*time.Second {
				delete(p.pending, seq)
				p.fails.Add(1)
				p.rtt.Store(-1)
			}
		}
		p.seq++
		seq := p.seq
		p.pending[seq] = time.Now()
		p.mu.Unlock()
		select {
		case p.inject <- echoRequest(p.src, p.dst, p.id, seq):
		default:
		}
	}
}

// echoRequest builds an IPv4 ICMP echo request with a 16-byte payload.
func echoRequest(src, dst netip.Addr, id, seq uint16) []byte {
	b := make([]byte, 20+8+16)
	b[0], b[8], b[9] = 0x45, 64, 1 // version/IHL, TTL, protocol ICMP
	binary.BigEndian.PutUint16(b[2:], uint16(len(b)))
	s4, d4 := src.As4(), dst.As4()
	copy(b[12:16], s4[:])
	copy(b[16:20], d4[:])
	binary.BigEndian.PutUint16(b[10:], checksum(b[:20]))
	icmp := b[20:]
	icmp[0] = 8 // echo request
	binary.BigEndian.PutUint16(icmp[4:], id)
	binary.BigEndian.PutUint16(icmp[6:], seq)
	copy(icmp[8:], "awgpilot-probe!!")
	binary.BigEndian.PutUint16(icmp[2:], checksum(icmp))
	return b
}

func checksum(b []byte) uint16 {
	var s uint32
	for i := 0; i+1 < len(b); i += 2 {
		s += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	if len(b)%2 == 1 {
		s += uint32(b[len(b)-1]) << 8
	}
	for s>>16 != 0 {
		s = s&0xffff + s>>16
	}
	return ^uint16(s)
}
