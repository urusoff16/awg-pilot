// Package awgprobe checks an AmneziaWG server without bringing the VPN up: a separate device on a
// user-space TCP/IP stack makes a handshake and fetches a small page through the server, like the
// delay checks of subscription servers. It is how AWG Pilot learns whether the main profile works
// again while another one carries the traffic.
//
// The check uses the profile's own key, so it must not run while the same profile is the tunnel
// in use: the server would move its endpoint to the probe.
package awgprobe

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/conn"
	"github.com/amnezia-vpn/amneziawg-go/v3/device"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun/netstack"

	"awgpilot/internal/awgconf"
	"awgpilot/internal/netx"
)

// ProbeURL answers 204 quickly from everywhere (the same page as the subscription checks).
const ProbeURL = "http://cp.cloudflare.com/generate_204"

// Probe measures the server of an AmneziaWG config: it returns the time of a request through the
// server after the handshake, in milliseconds. pin, if set, is called with the device's socket
// binding once it is open (Windows pins it to the physical network, past another VPN).
func Probe(ctx context.Context, raw string, pin func(conn.Bind) error) (int64, error) {
	cfg, err := awgconf.Parse(raw)
	if err != nil {
		return -1, err
	}
	endpoint, err := resolve(ctx, cfg.Peer.Endpoint)
	if err != nil {
		return -1, err
	}
	target, err := probeTarget(ctx)
	if err != nil {
		return -1, err
	}
	var local []netip.Addr
	for _, p := range cfg.Addresses {
		if p.Addr().Is4() {
			local = append(local, p.Addr())
		}
	}
	if len(local) == 0 {
		return -1, errors.New("в конфиге нет адреса IPv4")
	}
	mtu := cfg.MTU
	if mtu == 0 {
		mtu = 1280
	}
	td, tnet, err := netstack.CreateNetTUN(local, nil, mtu)
	if err != nil {
		return -1, err
	}
	bind := conn.NewDefaultBind()
	dev := device.NewDevice(td, bind, &device.Logger{Verbosef: device.DiscardLogf, Errorf: device.DiscardLogf})
	defer dev.Close()
	// Only IPv4 through the server; no keepalives: the check is over in seconds.
	cfg.Peer.Keepalive = "0"
	if err := dev.IpcSet(cfg.UAPI(endpoint, []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")})); err != nil {
		return -1, fmt.Errorf("конфиг не принят: %w", err)
	}
	if err := dev.Up(); err != nil {
		return -1, err
	}
	if pin != nil {
		if err := pin(bind); err != nil {
			return -1, err
		}
	}
	// The first request includes the handshake; the second is the delay a user would feel.
	var best int64 = -1
	var last error
	for try := 0; try < 3 && ctx.Err() == nil; try++ {
		ms, err := fetch(ctx, tnet, target)
		if err != nil {
			last = err
			continue
		}
		if try > 0 || best < 0 {
			best = ms
		}
		if try > 0 {
			return best, nil
		}
	}
	if best > 0 {
		return best, nil
	}
	if last == nil {
		last = ctx.Err()
	}
	return -1, fmt.Errorf("сервер не ответил: %w", last)
}

func fetch(ctx context.Context, tnet *netstack.Net, target netip.AddrPort) (int64, error) {
	tctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	start := time.Now()
	c, err := tnet.DialContextTCPAddrPort(tctx, target)
	if err != nil {
		return -1, err
	}
	defer c.Close()
	stop := context.AfterFunc(tctx, func() { c.Close() })
	defer stop()
	req, _ := http.NewRequestWithContext(tctx, http.MethodGet, ProbeURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Close = true
	if err := req.Write(c); err != nil {
		return -1, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), req)
	if err != nil {
		return -1, err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return -1, fmt.Errorf("ответ %d", resp.StatusCode)
	}
	return max(time.Since(start).Milliseconds(), 1), nil
}

// resolve turns the config's endpoint into ip:port (IPv4 first).
func resolve(ctx context.Context, ep string) (string, error) {
	host, port, err := net.SplitHostPort(ep)
	if err != nil {
		return "", fmt.Errorf("неверный адрес сервера %q", ep)
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return ep, nil
	}
	rctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	ips, err := netx.Resolver.LookupNetIP(rctx, "ip4", host)
	if err != nil || len(ips) == 0 {
		return "", fmt.Errorf("не удалось найти адрес сервера %s", host)
	}
	return net.JoinHostPort(ips[0].Unmap().String(), port), nil
}

// probeTarget is the address of ProbeURL's host, found outside the probe (the probe's stack has
// no DNS of its own).
func probeTarget(ctx context.Context) (netip.AddrPort, error) {
	rctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	ips, err := netx.Resolver.LookupNetIP(rctx, "ip4", "cp.cloudflare.com")
	if err != nil || len(ips) == 0 {
		return netip.AddrPort{}, errors.New("адрес страницы проверки не найден")
	}
	return netip.AddrPortFrom(ips[0].Unmap(), 80), nil
}
