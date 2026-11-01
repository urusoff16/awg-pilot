package main

import (
	"bufio"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/conn"
	"github.com/amnezia-vpn/amneziawg-go/v3/device"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
)

// tunnel is one running amneziawg-go device on a VpnService interface.
type tunnel struct {
	dev       *device.Device
	pt        *pingTun
	since     time.Time
	endpoint  string
	v6Blocked bool
	mtu       int
}

// startTunnel takes ownership of fd (closed on failure). tap, if set, sees the apps' packets
// (not the app's own pings and speed test, which are added and removed above it).
func startTunnel(fd int, uapi string, mtu int, src, pingTo netip.Addr, tap packetTap, logf func(string, ...any)) (*tunnel, error) {
	td, err := tunFromFD(fd)
	if err != nil {
		return nil, fmt.Errorf("интерфейс VPN не открылся: %w", err)
	}
	if tap != nil {
		td = &tapTun{Device: td, tap: tap}
	}
	pt := newPingTun(td, src, pingTo)
	var lastErr time.Time
	logger := &device.Logger{
		Verbosef: device.DiscardLogf,
		Errorf: func(format string, args ...any) {
			if time.Since(lastErr) > 30*time.Second { // a dead network repeats the same error every packet
				lastErr = time.Now()
				logf("awg: "+format, args...)
			}
		},
	}
	dev := device.NewDevice(pt, conn.NewDefaultBind(), logger)
	if err := dev.IpcSet(uapi); err != nil {
		dev.Close()
		return nil, fmt.Errorf("конфиг не принят: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("туннель не запустился: %w", err)
	}
	return &tunnel{dev: dev, pt: pt, since: time.Now()}, nil
}

func (t *tunnel) Close() { t.dev.Close() }

func (t *tunnel) Rebind() {
	if err := t.dev.BindUpdate(); err == nil {
		t.dev.SendKeepalivesToPeersWithCurrentKeypair()
	}
}

func (t *tunnel) Stats() Stats {
	s := Stats{Up: true, Since: t.since, Endpoint: t.endpoint, IPv6Blocked: t.v6Blocked,
		PingMs: t.pt.rtt.Load(), PingFails: int(t.pt.fails.Load())}
	uapi, err := t.dev.IpcGet()
	if err != nil {
		return s
	}
	var sec, nsec int64
	sc := bufio.NewScanner(strings.NewReader(uapi))
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), "=")
		switch k {
		case "rx_bytes":
			n, _ := strconv.ParseUint(v, 10, 64)
			s.RxBytes += n
		case "tx_bytes":
			n, _ := strconv.ParseUint(v, 10, 64)
			s.TxBytes += n
		case "last_handshake_time_sec":
			sec, _ = strconv.ParseInt(v, 10, 64)
		case "last_handshake_time_nsec":
			nsec, _ = strconv.ParseInt(v, 10, 64)
		case "endpoint":
			s.Endpoint = v
		}
	}
	if sec > 0 {
		s.LastHandshake = time.Unix(sec, nsec)
	}
	return s
}

// packetTap is the traffic log (flows.Recorder).
type packetTap interface {
	Packet(pkt []byte, out bool)
}

// tapTun shows packets to the traffic log: Read takes what apps send, Write delivers replies.
type tapTun struct {
	tun.Device
	tap packetTap
}

func (t *tapTun) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	n, err := t.Device.Read(bufs, sizes, offset)
	for i := 0; i < n; i++ {
		t.tap.Packet(bufs[i][offset:offset+sizes[i]], true)
	}
	return n, err
}

func (t *tapTun) Write(bufs [][]byte, offset int) (int, error) {
	for _, b := range bufs {
		t.tap.Packet(b[offset:], false)
	}
	return t.Device.Write(bufs, offset)
}
