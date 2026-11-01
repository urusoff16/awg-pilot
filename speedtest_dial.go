package main

import (
	"context"
	"errors"
	"math/bits"
	"net"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"awgpilot/internal/netx"
)

// otherVPN reports the adapter that carries internet traffic when it is not ours: with another
// VPN (AmneziaVPN, ...) connected too, its routes win and AWG Pilot's tunnel stays idle. It only
// asks the routing table (a UDP "connect" sends nothing) and is meant for full/exclude modes.
func otherVPN() string {
	c, err := net.Dial("udp4", "1.1.1.1:53")
	if err != nil {
		return ""
	}
	local := c.LocalAddr().(*net.UDPAddr).IP
	c.Close()
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.Equal(local) {
				if ifc.Name == "AWGPilot" {
					return ""
				}
				if ifc.Flags&net.FlagPointToPoint != 0 || isVirtualVPN(ifc.Name) {
					return ifc.Name
				}
				return "" // the physical network: e.g. the tunnel is still coming up
			}
		}
	}
	return ""
}

func isVirtualVPN(name string) bool {
	n := strings.ToLower(name)
	for _, s := range []string{"tun", "tap", "wireguard", "amnezia", "wintun", "vpn", "outline", "warp"} {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// ipUnicastIF is IP_UNICAST_IF from ws2ipdef.h (not exported by x/sys/windows).
const ipUnicastIF = 31

// tunnelDialer opens IPv4 connections that leave through the AWG Pilot adapter regardless of
// the routing table, so the speed test measures the VPN even in split-tunnel modes.
func tunnelDialer() (func(ctx context.Context, network, addr string) (net.Conn, error), error) {
	ifc, err := net.InterfaceByName("AWGPilot")
	if err != nil {
		return nil, errors.New("адаптер VPN не найден — подключите VPN")
	}
	idx := int(bits.ReverseBytes32(uint32(ifc.Index))) // the option takes the index in network byte order
	d := &net.Dialer{Timeout: 15 * time.Second, Resolver: netx.Resolver,
		Control: func(network, address string, c syscall.RawConn) error {
			var serr error
			if err := c.Control(func(fd uintptr) {
				serr = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, ipUnicastIF, idx)
			}); err != nil {
				return err
			}
			return serr
		}}
	return func(ctx context.Context, _, addr string) (net.Conn, error) {
		c, err := d.DialContext(ctx, "tcp4", addr)
		if errors.Is(err, windows.WSAEACCES) {
			return nil, errors.New("Windows не пускает соединение через VPN — похоже, включён другой VPN (например, AmneziaVPN); отключите его")
		}
		return c, err
	}, nil
}
