package tunnel

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
	"github.com/amnezia-vpn/amneziawg-windows/conf"
	"github.com/amnezia-vpn/amneziawg-windows/tunnel/winipcfg"
	"golang.org/x/sys/windows"

	"awgpilot/internal/wfp"
	"awgpilot/internal/xray"
)

// proxyAddr is the adapter's own address in proxy mode: Xray's stack answers for every
// destination, so it only has to be an address nothing else uses.
var proxyAddr = conf.IPCidr{IP: net.IPv4(172, 19, 0, 1).To4(), Cidr: 30}

// defaultProxyMTU: proxied packets are not wrapped again, so the usual Ethernet size fits.
const defaultProxyMTU = 1500

// upProxy brings the adapter up with the traffic going through an Xray server (o.Proxy).
// Callers hold m.mu; on error the caller's teardown runs.
func (m *Manager) upProxy(o Options, logf func(string, ...any)) error {
	link, err := xray.ParseLink(o.Proxy)
	if err != nil {
		return fmt.Errorf("ссылка на сервер повреждена: %w", err)
	}
	cfg := &conf.Config{Name: adapterName}
	cfg.Interface.Addresses = []conf.IPCidr{proxyAddr}
	for _, a := range o.DNS {
		cfg.Interface.DNS = append(cfg.Interface.DNS, net.IP(a.AsSlice()))
	}
	if len(cfg.Interface.DNS) == 0 {
		cfg.Interface.DNS = []net.IP{net.IPv4(1, 1, 1, 1), net.IPv4(1, 0, 0, 1)}
	}
	cfg.Interface.MTU = uint16(o.MTU)
	if o.MTU <= 0 {
		cfg.Interface.MTU = defaultProxyMTU
	}
	peer := conf.Peer{}
	if len(o.AllowedIPs) == 0 {
		peer.AllowedIPs = []conf.IPCidr{{IP: net.IPv4zero.To4(), Cidr: 0}}
	}
	for _, p := range o.AllowedIPs {
		if p.Addr().Is4() {
			peer.AllowedIPs = append(peer.AllowedIPs, conf.IPCidr{IP: net.IP(p.Addr().AsSlice()), Cidr: uint8(p.Bits())})
		}
	}
	cfg.Peers = []conf.Peer{peer}
	blockV6 := o.BlockIPv6

	tunDev, err := tun.CreateTUNWithRequestedGUID(adapterName, &adapterGUID, 0)
	if err != nil {
		return fmt.Errorf("не удалось создать сетевой адаптер wintun (нужны права администратора, wintun.dll должен лежать рядом с программой): %w", err)
	}
	m.nativeTun = tunDev.(*tun.NativeTun)
	m.nativeTun.ForceMTU(int(cfg.Interface.MTU))

	if o.KillSwitch && o.FullTunnel {
		if err := wfp.EnableFirewall(m.nativeTun.LUID(), false, cfg.Interface.DNS); err != nil {
			return fmt.Errorf("не удалось включить kill switch: %w", err)
		}
		m.firewall = true
	}

	// Xray's sockets to the server must leave through the physical interface, never the tunnel.
	m.binder = &xray.Binder{}
	for _, family := range []winipcfg.AddressFamily{windows.AF_INET, windows.AF_INET6} {
		hasDefault := o.FullTunnel || (family == windows.AF_INET6 && blockV6)
		cbs, err := monitorDefaultRoutes(family, m.binder, hasDefault, true, m.nativeTun, logf, m.netSwitched(family))
		if err != nil {
			return fmt.Errorf("не удалось отследить основной сетевой интерфейс: %w", err)
		}
		m.callbacks = append(m.callbacks, cbs...)
	}
	xray.SetSocketHook(m.binder.Hook)

	inst, err := xray.Start(link, o.ProxyAddr, o.Direct...)
	if err != nil {
		return err
	}
	var dev tun.Device = tunDev
	if m.Tap != nil {
		dev = &tapTun{Device: tunDev, tap: m.Tap}
	}
	eng, err := xray.NewEngine(inst, dev, xray.EngineOptions{MTU: int(cfg.Interface.MTU), DropIPv6: blockV6, Sniff: len(o.Direct) > 0})
	if err != nil {
		inst.Close()
		return fmt.Errorf("не удалось запустить Xray: %w", err)
	}
	m.proxy = eng
	m.proxyEndpoint = link.Endpoint()
	m.proxyName = link.Name

	for _, family := range []winipcfg.AddressFamily{windows.AF_INET, windows.AF_INET6} {
		if err := configureInterface(family, cfg, m.nativeTun, blockV6 && family == windows.AF_INET6); err != nil {
			return fmt.Errorf("не удалось настроить сетевой адаптер: %w", err)
		}
	}
	m.since = time.Now()
	m.v6Blocked = blockV6
	logf("туннель поднят через Xray: сервер %s (%s, %s), маршрутов %d, kill switch: %s, IPv6: %s", link.Name, link.Proto, m.proxyEndpoint,
		len(peer.AllowedIPs), onOff(m.firewall), map[bool]string{true: "заблокирован", false: "без изменений"}[blockV6])
	return nil
}

// ProbeProxy measures the running proxy server: a request through it, like the server check.
// It returns an error when the tunnel is not a proxy one.
func (m *Manager) ProbeProxy(ctx context.Context) (time.Duration, error) {
	m.mu.Lock()
	eng := m.proxy
	m.mu.Unlock()
	if eng == nil {
		return 0, errNotProxy
	}
	d, err := eng.Instance().Delay(ctx)
	if err == nil {
		m.proxyOK.Store(time.Now().UnixNano())
	}
	return d, err
}

// IsProxy reports whether the running tunnel goes through an Xray server.
func (m *Manager) IsProxy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.proxy != nil
}

var errNotProxy = fmt.Errorf("туннель работает не через сервер подписки")

// NetKind is the kind of the physical network that carries the traffic: "wifi", "mobile",
// "wired", or "" when unknown or offline.
func NetKind() string {
	r, err := winipcfg.GetIPForwardTable2(windows.AF_INET)
	if err != nil {
		return ""
	}
	best, bestMetric := winipcfg.LUID(0), ^uint32(0)
	for i := range r {
		if r[i].DestinationPrefix.PrefixLength != 0 {
			continue
		}
		ifrow, err := r[i].InterfaceLUID.Interface()
		if err != nil || ifrow.OperStatus != winipcfg.IfOperStatusUp || ifrow.Alias() == adapterName || isVirtual(ifrow.Type) {
			continue
		}
		iface, err := r[i].InterfaceLUID.IPInterface(windows.AF_INET)
		if err != nil {
			continue
		}
		if m := r[i].Metric + iface.Metric; m < bestMetric {
			best, bestMetric = r[i].InterfaceLUID, m
		}
	}
	if best == 0 {
		return ""
	}
	return kindOf(best)
}

// isVirtual reports adapters of VPNs and other software (WireGuard, wintun, Hyper-V's internal
// switch, tunnels), as opposed to Wi-Fi, cable and modems.
func isVirtual(t winipcfg.IfType) bool {
	return t == winipcfg.IfTypePropVirtual || t == winipcfg.IfTypeTunnel
}

func kindOf(luid winipcfg.LUID) string {
	row, err := luid.Interface()
	if err != nil {
		return ""
	}
	switch row.Type {
	case 71: // IF_TYPE_IEEE80211
		return "wifi"
	case 243, 244: // IF_TYPE_WWANPP, IF_TYPE_WWANPP2: built-in or USB modems
		return "mobile"
	case 6: // IF_TYPE_ETHERNET_CSMACD, including phones shared over USB
		desc := row.Description()
		for _, w := range []string{"RNDIS", "Remote NDIS", "Android", "iPhone", "Apple Mobile", "Modem", "HUAWEI", "ZTE"} {
			if strings.Contains(strings.ToLower(desc), strings.ToLower(w)) {
				return "mobile"
			}
		}
		return "wired"
	}
	return ""
}

// indexBinder records the interface bindSocketRoute chooses instead of binding anything.
type indexBinder struct{ v4, v6 uint32 }

func (b *indexBinder) BindSocketToInterface4(index uint32, _ bool) error { b.v4 = index; return nil }
func (b *indexBinder) BindSocketToInterface6(index uint32, _ bool) error { b.v6 = index; return nil }

// PhysicalIndex returns the interfaces (IPv4, IPv6; 0 = none) of the best default routes over the
// physical network: not this program's adapter, nor another VPN's while a physical one exists.
func (m *Manager) PhysicalIndex() (v4, v6 uint32) {
	m.mu.Lock()
	ours := winipcfg.LUID(0)
	if m.nativeTun != nil {
		ours = winipcfg.LUID(m.nativeTun.LUID())
	}
	m.mu.Unlock()
	b := &indexBinder{}
	for _, family := range []winipcfg.AddressFamily{windows.AF_INET, windows.AF_INET6} {
		last, lastIndex := winipcfg.LUID(0), ^uint32(0)
		bindSocketRoute(family, b, ours, &last, &lastIndex, false, true, func(string, ...any) {}, nil)
	}
	return b.v4, b.v6
}

// WithPhysicalSockets runs f with Xray's sockets pinned to the physical network, as they are
// while a subscription tunnel runs. Server checks then measure the path the tunnel will take,
// not another VPN that happens to hold the default route (its servers may be reachable through
// that VPN and blocked straight from the provider, or the other way round).
func (m *Manager) WithPhysicalSockets(f func()) {
	m.mu.Lock()
	if m.binder != nil { // a proxy tunnel runs: its binding already applies
		m.mu.Unlock()
		f()
		return
	}
	ours := winipcfg.LUID(0)
	if m.nativeTun != nil {
		ours = winipcfg.LUID(m.nativeTun.LUID())
	}
	b := &xray.Binder{}
	for _, family := range []winipcfg.AddressFamily{windows.AF_INET, windows.AF_INET6} {
		last, lastIndex := winipcfg.LUID(0), ^uint32(0)
		bindSocketRoute(family, b, ours, &last, &lastIndex, false, true, func(string, ...any) {}, nil)
	}
	xray.SetSocketHook(b.Hook)
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		if m.binder == nil { // a tunnel that came up meanwhile keeps its own
			xray.SetSocketHook(nil)
		}
		m.mu.Unlock()
	}()
	f()
}
