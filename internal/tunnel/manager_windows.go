// Package tunnel runs an AmneziaWG tunnel in-process on Windows (wintun + amneziawg-go).
//
// Interface/route/DNS configuration and default-route socket binding are adapted from
// amneziawg-windows/tunnel (MIT, Copyright (C) 2019-2021 WireGuard LLC).
package tunnel

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"awgpilot/internal/awgconf"
	"awgpilot/internal/wfp"
	"github.com/amnezia-vpn/amneziawg-go/v3/conn"
	"github.com/amnezia-vpn/amneziawg-go/v3/device"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
	"github.com/amnezia-vpn/amneziawg-windows/conf"
	"github.com/amnezia-vpn/amneziawg-windows/tunnel/winipcfg"
	"golang.org/x/sys/windows"
)

const adapterName = "AWGPilot"

// Fixed GUID so Windows reuses one network profile instead of creating "Network 2, 3, ...".
var adapterGUID = windows.GUID{Data1: 0x6f1c9a52, Data2: 0x3b7e, Data3: 0x4d21, Data4: [8]byte{0x9a, 0x41, 0x0c, 0x5e, 0x77, 0x2b, 0xd3, 0x18}}

// Options control how a parsed config is brought up.
type Options struct {
	RawConfig  string
	AllowedIPs []netip.Prefix // replaces the peer's AllowedIPs when non-empty
	FullTunnel bool
	KillSwitch bool         // only honoured when FullTunnel
	DNS        []netip.Addr // replaces config DNS when non-empty
	MTU        int          // 0 = keep config value
	// BlockIPv6 routes all IPv6 into the tunnel when the config has no IPv6 address; the device
	// drops it (no peer accepts IPv6), so apps fall back to IPv4 inside the VPN instead of leaking.
	BlockIPv6 bool
	// Lockdown is handled by the service (internet stays closed if the tunnel drops);
	// Manager itself ignores it.
	Lockdown bool
	Logf     func(format string, args ...any)
}

// ULA address given to the adapter so Windows will use the IPv6 blackhole route.
var blackholeV6Addr = conf.IPCidr{IP: net.ParseIP("fd8a:7c1f:4e2d::2"), Cidr: 128}

// Stats is a snapshot of the running tunnel.
type Stats struct {
	Up            bool      `json:"up"`
	Endpoint      string    `json:"endpoint"`
	LastHandshake time.Time `json:"last_handshake"`
	RxBytes       uint64    `json:"rx_bytes"`
	TxBytes       uint64    `json:"tx_bytes"`
	Since         time.Time `json:"since"`
	KillSwitch    bool      `json:"kill_switch"`
	IPv6Blocked   bool      `json:"ipv6_blocked"`
	NetChanges    uint64    `json:"net_changes"` // physical network switches seen (Wi-Fi, cable, hotspot)
	NoNetwork     bool      `json:"no_network"`  // no physical default route: nothing to reconnect over

	// Filled in by the service, not by Manager:
	Blocked   bool  `json:"blocked"`    // internet closed because the VPN dropped
	Lockdown  bool  `json:"lockdown"`   // "disconnect the internet when the VPN drops" is armed
	PingMs    int64 `json:"ping_ms"`    // last round trip through the tunnel; -1 = no reply
	PingFails int   `json:"ping_fails"` // consecutive failed pings
}

// PacketTap sees every packet the tunnel carries: out=true leaving the computer, false arriving.
// It runs on the packet path, so it must be quick.
type PacketTap interface {
	Packet(pkt []byte, out bool)
}

type Manager struct {
	// Tap, when set before Up, gets the tunnel's packets (the traffic log).
	Tap PacketTap

	mu        sync.Mutex
	dev       *device.Device
	nativeTun *tun.NativeTun
	callbacks []interface{ Unregister() error }
	firewall  bool
	v6Blocked bool
	since     time.Time

	netChanges atomic.Uint64
	noNet      atomic.Bool
}

// LUID of the running tunnel adapter, or 0 when down.
func (m *Manager) LUID() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dev == nil || m.nativeTun == nil {
		return 0
	}
	return m.nativeTun.LUID()
}

// ParseConfig validates an AmneziaWG wg-quick config. The AmneziaWG 3.x keys are checked here
// but left out of the result (the amneziawg-windows parser knows only 2.0); Up passes them to
// the device separately.
func ParseConfig(raw string) (*conf.Config, error) {
	rest, _, err := awgconf.SplitExtended(raw)
	if err != nil {
		return nil, err
	}
	c, err := conf.FromWgQuickWithUnknownEncoding(rest, adapterName)
	if err != nil {
		return nil, err
	}
	if len(c.Peers) != 1 {
		return nil, fmt.Errorf("в конфиге должен быть ровно один раздел [Peer], найдено %d", len(c.Peers))
	}
	return c, nil
}

// Up starts the tunnel; it fails if one is already running.
func (m *Manager) Up(o Options) (err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dev != nil {
		return errors.New("туннель уже запущен")
	}
	logf := o.Logf
	if logf == nil {
		logf = log.Printf
	}
	cfg, err := ParseConfig(o.RawConfig)
	if err != nil {
		return fmt.Errorf("конфиг повреждён: %w", err)
	}
	// The provider may issue an IPv6 address; then IPv6 goes through the tunnel as the config
	// says. Without one, IPv6 is blackholed (BlockIPv6) or left alone.
	hasV6Addr := false
	for _, a := range cfg.Interface.Addresses {
		if a.Bits() == 128 || a.IP.To4() == nil {
			hasV6Addr = true
		}
	}
	if len(o.AllowedIPs) > 0 {
		// The split plan covers IPv4 only, so keep the config's IPv6 ranges: dropping them would
		// send IPv6 from a provider-issued address straight to the physical network.
		var keepV6 []conf.IPCidr
		if hasV6Addr {
			for _, a := range cfg.Peers[0].AllowedIPs {
				if a.IP.To4() == nil {
					keepV6 = append(keepV6, a)
				}
			}
		}
		cfg.Peers[0].AllowedIPs = cfg.Peers[0].AllowedIPs[:0]
		for _, p := range o.AllowedIPs {
			cfg.Peers[0].AllowedIPs = append(cfg.Peers[0].AllowedIPs, conf.IPCidr{IP: net.IP(p.Addr().AsSlice()), Cidr: uint8(p.Bits())})
		}
		cfg.Peers[0].AllowedIPs = append(cfg.Peers[0].AllowedIPs, keepV6...)
	}
	v6Default := false // real IPv6 with the default route through the tunnel
	for _, a := range cfg.Peers[0].AllowedIPs {
		if hasV6Addr && a.IP.To4() == nil && a.Cidr == 0 {
			v6Default = true
		}
	}
	if hasV6Addr && o.BlockIPv6 && !v6Default {
		// The config routes only part of IPv6 (or none): send the rest into the tunnel too, so
		// nothing leaves beside it on a dual-stack network.
		cfg.Peers[0].AllowedIPs = append(cfg.Peers[0].AllowedIPs, conf.IPCidr{IP: net.IPv6zero, Cidr: 0})
		v6Default = true
	}
	if len(o.DNS) > 0 {
		cfg.Interface.DNS = cfg.Interface.DNS[:0]
		for _, a := range o.DNS {
			cfg.Interface.DNS = append(cfg.Interface.DNS, net.IP(a.AsSlice()))
		}
	}
	if o.MTU > 0 {
		cfg.Interface.MTU = uint16(o.MTU)
	}
	blockV6 := o.BlockIPv6 && !hasV6Addr

	defer func() {
		if err != nil {
			m.teardownLocked()
		}
	}()

	tunDev, err := tun.CreateTUNWithRequestedGUID(adapterName, &adapterGUID, 0)
	if err != nil {
		return fmt.Errorf("не удалось создать сетевой адаптер wintun (нужны права администратора, wintun.dll должен лежать рядом с программой): %w", err)
	}
	m.nativeTun = tunDev.(*tun.NativeTun)

	logger := &device.Logger{
		Verbosef: func(string, ...any) {},
		Errorf:   func(f string, a ...any) { logf("[ядро AWG] "+f, a...) },
	}
	var dev tun.Device = tunDev
	if m.Tap != nil {
		dev = &tapTun{Device: tunDev, tap: m.Tap}
	}
	m.dev = device.NewDevice(dev, conn.NewDefaultBind(), logger)

	// Order follows amneziawg-windows/tunnel/service.go: firewall, config, Up, then socket
	// binding and interface setup (the UDP socket only exists after Up).
	if o.KillSwitch && o.FullTunnel {
		if err = wfp.EnableFirewall(m.nativeTun.LUID(), false, cfg.Interface.DNS); err != nil {
			return fmt.Errorf("не удалось включить kill switch: %w", err)
		}
		m.firewall = true
	}

	uapi, dnsErr := cfg.ToUAPI()
	if dnsErr != nil {
		return fmt.Errorf("не удалось определить адрес сервера: %w", dnsErr)
	}
	// AmneziaWG 3.x device settings go before the first peer.
	_, ext, _ := awgconf.SplitExtended(o.RawConfig)
	if ext.Keepalive != "" { // a 3.x range the 2.0 parser could not hold
		uapi = strings.Replace(uapi, "persistent_keepalive_interval=0\n", "persistent_keepalive_interval="+ext.Keepalive+"\n", 1)
	}
	if len(ext.Device) > 0 {
		at := strings.Index(uapi, "replace_peers=")
		if at < 0 {
			at = len(uapi)
		}
		uapi = uapi[:at] + strings.Join(ext.Device, "\n") + "\n" + uapi[at:]
	}
	if err = m.dev.IpcSet(uapi); err != nil {
		return fmt.Errorf("ядро AmneziaWG не приняло конфиг: %w", err)
	}
	if err = m.dev.Up(); err != nil {
		return fmt.Errorf("не удалось запустить туннель: %w", err)
	}

	if binder, ok := m.dev.Bind().(conn.BindSocketToInterface); ok {
		for _, family := range []winipcfg.AddressFamily{windows.AF_INET, windows.AF_INET6} {
			// With a default route through the tunnel, losing the physical route must not let
			// encrypted packets loop back into the tunnel: blackhole them instead.
			hasDefault := o.FullTunnel || (family == windows.AF_INET6 && (blockV6 || v6Default))
			cbs, err := monitorDefaultRoutes(family, binder, hasDefault, m.nativeTun, logf, m.netSwitched(family))
			if err != nil {
				return fmt.Errorf("не удалось отследить основной сетевой интерфейс: %w", err)
			}
			m.callbacks = append(m.callbacks, cbs...)
		}
	}

	for _, family := range []winipcfg.AddressFamily{windows.AF_INET, windows.AF_INET6} {
		if err = configureInterface(family, cfg, m.nativeTun, blockV6 && family == windows.AF_INET6); err != nil {
			return fmt.Errorf("не удалось настроить сетевой адаптер: %w", err)
		}
	}
	m.since = time.Now()
	m.v6Blocked = blockV6
	logf("туннель поднят: сервер %s, маршрутов %d, kill switch: %s, IPv6: %s", cfg.Peers[0].Endpoint.String(), len(cfg.Peers[0].AllowedIPs), onOff(m.firewall), map[bool]string{true: "заблокирован", false: "без изменений"}[blockV6])
	return nil
}

// tapTun shows packets to a PacketTap on their way between Windows and the tunnel: Read takes
// what applications send, Write delivers what came from the server.
type tapTun struct {
	tun.Device
	tap PacketTap
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

// netSwitched records a change of the physical interface and nudges the server at once, so it
// learns the new address instead of waiting for the next keepalive.
func (m *Manager) netSwitched(family winipcfg.AddressFamily) func(index uint32) {
	dev := m.dev
	return func(index uint32) {
		if family == windows.AF_INET {
			m.noNet.Store(index == 0)
		}
		if index != 0 {
			m.netChanges.Add(1)
			go dev.SendKeepalivesToPeersWithCurrentKeypair()
		}
	}
}

func onOff(b bool) string {
	if b {
		return "вкл"
	}
	return "выкл"
}

// Down stops the tunnel and removes routes, DNS and firewall rules.
func (m *Manager) Down() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.teardownLocked()
}

func (m *Manager) teardownLocked() {
	for _, cb := range m.callbacks {
		cb.Unregister()
	}
	m.callbacks = nil
	if m.firewall {
		wfp.DisableFirewall()
		m.firewall = false
	}
	if m.dev != nil {
		m.dev.Close() // also closes the TUN adapter, which drops its routes and DNS
		m.dev = nil
	} else if m.nativeTun != nil {
		m.nativeTun.Close()
	}
	m.nativeTun = nil
	m.since = time.Time{}
	m.v6Blocked = false
	m.noNet.Store(false)
}

func (m *Manager) IsUp() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dev != nil
}

// Stats reads counters from the device's UAPI.
func (m *Manager) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Stats{Up: m.dev != nil, Since: m.since, KillSwitch: m.firewall, IPv6Blocked: m.v6Blocked,
		NetChanges: m.netChanges.Load(), NoNetwork: m.dev != nil && m.noNet.Load()}
	if m.dev == nil {
		return st
	}
	out, err := m.dev.IpcGet()
	if err != nil {
		return st
	}
	var sec, nsec int64
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch k {
		case "endpoint":
			st.Endpoint = v
		case "rx_bytes":
			st.RxBytes, _ = strconv.ParseUint(v, 10, 64)
		case "tx_bytes":
			st.TxBytes, _ = strconv.ParseUint(v, 10, 64)
		case "last_handshake_time_sec":
			sec, _ = strconv.ParseInt(v, 10, 64)
		case "last_handshake_time_nsec":
			nsec, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	if sec > 0 {
		st.LastHandshake = time.Unix(sec, nsec)
	}
	return st
}

// bindSocketRoute binds the tunnel's UDP socket to the best physical default route so
// encrypted traffic never loops back into the tunnel.
func bindSocketRoute(family winipcfg.AddressFamily, binder conn.BindSocketToInterface, ourLUID winipcfg.LUID, lastLUID *winipcfg.LUID, lastIndex *uint32, blackholeWhenLoop bool, logf func(string, ...any), switched func(index uint32)) error {
	r, err := winipcfg.GetIPForwardTable2(family)
	if err != nil {
		return err
	}
	lowestMetric := ^uint32(0)
	index := uint32(0)
	luid := winipcfg.LUID(0)
	for i := range r {
		if r[i].DestinationPrefix.PrefixLength != 0 || r[i].InterfaceLUID == ourLUID {
			continue
		}
		ifrow, err := r[i].InterfaceLUID.Interface()
		if err != nil || ifrow.OperStatus != winipcfg.IfOperStatusUp {
			continue
		}
		iface, err := r[i].InterfaceLUID.IPInterface(family)
		if err != nil {
			continue
		}
		if r[i].Metric+iface.Metric < lowestMetric {
			lowestMetric = r[i].Metric + iface.Metric
			index = r[i].InterfaceIndex
			luid = r[i].InterfaceLUID
		}
	}
	if luid == *lastLUID && index == *lastIndex {
		return nil
	}
	first := *lastIndex == ^uint32(0)
	*lastLUID, *lastIndex = luid, index
	if switched != nil {
		if first {
			if index == 0 {
				switched(0)
			}
		} else {
			switched(index)
		}
	}
	blackhole := blackholeWhenLoop && index == 0
	if family == windows.AF_INET {
		logf("зашифрованный трафик IPv4 идёт через интерфейс %d%s", index, map[bool]string{true: " (сети нет — трафик удерживается)", false: ""}[blackhole])
		return binder.BindSocketToInterface4(index, blackhole)
	}
	logf("зашифрованный трафик IPv6 идёт через интерфейс %d%s", index, map[bool]string{true: " (сети нет — трафик удерживается)", false: ""}[blackhole])
	return binder.BindSocketToInterface6(index, blackhole)
}

func monitorDefaultRoutes(family winipcfg.AddressFamily, binder conn.BindSocketToInterface, blackholeWhenLoop bool, nt *tun.NativeTun, logf func(string, ...any), switched func(index uint32)) ([]interface{ Unregister() error }, error) {
	ourLUID := winipcfg.LUID(nt.LUID())
	lastLUID := winipcfg.LUID(0)
	lastIndex := ^uint32(0)
	var mu sync.Mutex
	doIt := func() {
		mu.Lock()
		defer mu.Unlock()
		if err := bindSocketRoute(family, binder, ourLUID, &lastLUID, &lastIndex, blackholeWhenLoop, logf, switched); err != nil {
			logf("не удалось переключить сетевой интерфейс: %v", err)
		}
	}
	if err := bindSocketRoute(family, binder, ourLUID, &lastLUID, &lastIndex, blackholeWhenLoop, logf, switched); err != nil {
		return nil, err
	}
	debounce := time.AfterFunc(time.Hour, doIt)
	debounce.Stop()
	bump := func() { debounce.Reset(150 * time.Millisecond) }

	cbr, err := winipcfg.RegisterRouteChangeCallback(func(_ winipcfg.MibNotificationType, route *winipcfg.MibIPforwardRow2) {
		if route != nil && route.DestinationPrefix.PrefixLength == 0 {
			bump()
		}
	})
	if err != nil {
		return nil, err
	}
	cbi, err := winipcfg.RegisterInterfaceChangeCallback(func(_ winipcfg.MibNotificationType, iface *winipcfg.MibIPInterfaceRow) {
		bump()
	})
	if err != nil {
		cbr.Unregister()
		return nil, err
	}
	return []interface{ Unregister() error }{cbr, cbi}, nil
}

// configureInterface sets addresses, routes, metric and DNS for one family. With blackholeV6 the
// IPv6 side gets a ULA address and a default route that the device silently drops.
func configureInterface(family winipcfg.AddressFamily, cfg *conf.Config, nt *tun.NativeTun, blackholeV6 bool) error {
	luid := winipcfg.LUID(nt.LUID())
	var addresses []net.IPNet
	var haveV4, haveV6 bool
	addrs := cfg.Interface.Addresses
	if blackholeV6 {
		addrs = append(append([]conf.IPCidr{}, addrs...), blackholeV6Addr)
	}
	for _, a := range addrs {
		if (a.Bits() == 32) != (family == windows.AF_INET) {
			continue
		}
		addresses = append(addresses, a.IPNet())
		if a.Bits() == 32 {
			haveV4 = true
		} else {
			haveV6 = true
		}
	}
	var routes []winipcfg.RouteData
	foundDefault := false
	for _, peer := range cfg.Peers {
		for _, aip := range peer.AllowedIPs {
			aip.MaskSelf()
			if (aip.Bits() == 32 && (family != windows.AF_INET || !haveV4)) || (aip.Bits() == 128 && (family != windows.AF_INET6 || !haveV6)) {
				continue
			}
			rd := winipcfg.RouteData{Destination: aip.IPNet(), Metric: 0, NextHop: net.IPv4zero}
			if aip.Bits() == 128 {
				rd.NextHop = net.IPv6zero
			}
			if aip.Cidr == 0 {
				foundDefault = true
			}
			routes = append(routes, rd)
		}
	}
	if blackholeV6 {
		routes = append(routes, winipcfg.RouteData{Destination: net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)}, NextHop: net.IPv6zero})
		foundDefault = true
	}
	err := luid.SetIPAddressesForFamily(family, addresses)
	if err == windows.ERROR_OBJECT_ALREADY_EXISTS {
		cleanupAddressesOnDisconnectedInterfaces(family, addresses)
		err = luid.SetIPAddressesForFamily(family, addresses)
	}
	if err != nil {
		return err
	}
	sort.Slice(routes, func(i, j int) bool {
		if c := bytes.Compare(routes[i].Destination.IP, routes[j].Destination.IP); c != 0 {
			return c < 0
		}
		return bytes.Compare(routes[i].Destination.Mask, routes[j].Destination.Mask) < 0
	})
	dedup := make([]*winipcfg.RouteData, 0, len(routes))
	for i := range routes {
		if i > 0 && bytes.Equal(routes[i].Destination.IP, routes[i-1].Destination.IP) && bytes.Equal(routes[i].Destination.Mask, routes[i-1].Destination.Mask) {
			continue
		}
		dedup = append(dedup, &routes[i])
	}
	if err := luid.SetRoutesForFamily(family, dedup); err != nil {
		return err
	}
	ipif, err := luid.IPInterface(family)
	if err != nil {
		return err
	}
	if cfg.Interface.MTU > 0 {
		ipif.NLMTU = uint32(cfg.Interface.MTU)
		nt.ForceMTU(int(ipif.NLMTU))
	}
	// Low interface metric makes Windows prefer the tunnel's DNS servers (no DNS leaks via the LAN resolver).
	ipif.UseAutomaticMetric = false
	ipif.Metric = 0
	if !foundDefault {
		ipif.Metric = 5
	}
	if family == windows.AF_INET6 {
		ipif.DadTransmits = 0
		ipif.RouterDiscoveryBehavior = winipcfg.RouterDiscoveryDisabled
	}
	if err := ipif.Set(); err != nil {
		return err
	}
	var dns []net.IP
	for _, d := range cfg.Interface.DNS {
		if (d.To4() != nil) == (family == windows.AF_INET) {
			dns = append(dns, d)
		}
	}
	return luid.SetDNS(family, dns, cfg.Interface.DNSSearch)
}

func cleanupAddressesOnDisconnectedInterfaces(family winipcfg.AddressFamily, addresses []net.IPNet) {
	interfaces, err := winipcfg.GetAdaptersAddresses(family, winipcfg.GAAFlagDefault)
	if err != nil {
		return
	}
	for _, iface := range interfaces {
		if iface.OperStatus == winipcfg.IfOperStatusUp {
			continue
		}
		for a := iface.FirstUnicastAddress; a != nil; a = a.Next {
			ip := a.Address.IP()
			n := net.IPNet{IP: ip, Mask: net.CIDRMask(int(a.OnLinkPrefixLength), 8*len(ip))}
			for _, want := range addresses {
				wip := want.IP
				if w4 := wip.To4(); w4 != nil {
					wip = w4
				}
				if bytes.Equal(wip, n.IP) && want.Mask.String() == n.Mask.String() {
					iface.LUID.DeleteIPAddress(n)
				}
			}
		}
	}
}
