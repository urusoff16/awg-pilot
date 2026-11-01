package main

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"awgpilot/internal/routing"
	"awgpilot/internal/store"
)

const testConf = `[Interface]
PrivateKey = YAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk=
Address = 10.8.1.23/32
DNS = 10.8.1.1
MTU = 1280
Jc = 4
S3 = 17
[Peer]
PublicKey = xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg=
AllowedIPs = 0.0.0.0/0
Endpoint = 193.0.2.7:35772
`

type fakePlatform struct{ plans []Plan }

func (f *fakePlatform) Establish(p Plan) (int, error) {
	f.plans = append(f.plans, p)
	return -1, errors.New("no VPN in tests")
}
func (f *fakePlatform) Release()                                   {}
func (f *fakePlatform) Notify(title, body string)                  {}
func (f *fakePlatform) Owner(int, string, int, string, int) string { return "" }

func testApp(t *testing.T, sdk int, s func(*store.Settings)) (*App, *fakePlatform) {
	dir := t.TempDir()
	if home, err := os.UserConfigDir(); err == nil { // reuse the Windows app's cached Russian list
		if b, err := os.ReadFile(filepath.Join(home, "AWGPilot", "ru-ipv4.txt")); err == nil {
			os.WriteFile(filepath.Join(dir, "ru-ipv4.txt"), b, 0o600)
		}
	}
	plat := &fakePlatform{}
	a, err := newApp(plat, dir, sdk, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { close(a.stop) })
	if err := a.ImportConfigText(testConf, "test.conf"); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	s(&a.settings)
	a.mu.Unlock()
	return a, plat
}

func TestPlanFullTunnel(t *testing.T) {
	a, plat := testApp(t, 34, func(s *store.Settings) { s.BlockIPv6 = true })
	if err := a.Connect(); err == nil {
		t.Fatal("expected the fake platform to refuse")
	}
	p := plat.plans[0]
	if strings.Join(p.Routes, ",") != "0.0.0.0/0,::/0" || len(p.ExcludeRoutes) != 0 {
		t.Fatalf("routes %v excl %v", p.Routes, p.ExcludeRoutes)
	}
	if strings.Join(p.Addresses, ",") != "10.8.1.23/32,"+blackholeV6 || strings.Join(p.DNS, ",") != "10.8.1.1" || p.MTU != 1280 || p.AppMode != "all" {
		t.Fatalf("plan %+v", p)
	}
}

func TestPlanBypass(t *testing.T) {
	if _, err := os.Stat(filepath.Join(must(os.UserConfigDir()), "AWGPilot", "ru-ipv4.txt")); err != nil {
		t.Skip("no cached Russian list")
	}
	dns := netip.MustParseAddr("10.8.1.1")
	for _, sdk := range []int{34, 28} {
		a, _ := testApp(t, sdk, func(s *store.Settings) {
			s.Split = routing.Settings{Mode: routing.ModeFull, BypassLAN: true, BypassRussia: true}
		})
		p, _, _, err := a.buildPlan()
		if err != nil {
			t.Fatal(err)
		}
		n := len(p.Routes) + len(p.ExcludeRoutes)
		t.Logf("sdk %d: %d routes, %d excluded", sdk, len(p.Routes), len(p.ExcludeRoutes))
		if n > routeBudget+2 { // + the default and IPv6 routes
			t.Errorf("sdk %d: %d routes exceed the budget", sdk, n)
		}
		inside := false
		for _, r := range p.Routes {
			if netip.MustParsePrefix(r).Contains(dns) && r != "0.0.0.0/0" {
				inside = true
			}
		}
		for _, r := range p.ExcludeRoutes {
			if netip.MustParsePrefix(r).Contains(dns) {
				t.Errorf("sdk %d: tunnel DNS excluded by %s", sdk, r)
			}
		}
		if sdk >= 33 && (len(p.ExcludeRoutes) == 0 || p.Routes[0] != "0.0.0.0/0") {
			t.Errorf("sdk %d should use excludeRoute", sdk)
		}
		if sdk < 33 && (len(p.ExcludeRoutes) != 0 || !inside) {
			t.Errorf("sdk %d should list included routes with the DNS inside", sdk)
		}
	}
}

func TestCallDispatch(t *testing.T) {
	a, _ := testApp(t, 34, func(*store.Settings) {})
	if r := a.Call("GetState", "[]"); !strings.Contains(r, `"ok":true`) || !strings.Contains(r, `"platform":"android"`) {
		t.Fatalf("GetState: %.200s", r)
	}
	if r := a.Call("RenameProfile", `["nope","x"]`); !strings.Contains(r, `"ok":false`) {
		t.Fatalf("RenameProfile: %s", r)
	}
	if r := a.Call("SaveSettings", `[{"app_mode":"include","app_packages":[]}]`); !strings.Contains(r, "выберите хотя бы одно") {
		t.Fatalf("SaveSettings: %s", r)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// With a provider-issued IPv6 address, IPv6 must go into the tunnel, not around it.
func TestPlanProviderIPv6(t *testing.T) {
	a, _ := testApp(t, 34, func(s *store.Settings) { s.BlockIPv6 = false })
	v6 := strings.Replace(strings.Replace(testConf, "Address = 10.8.1.23/32", "Address = 10.8.1.23/32, fd00:8::23/128", 1),
		"AllowedIPs = 0.0.0.0/0", "AllowedIPs = 0.0.0.0/0, ::/0", 1)
	if err := a.ImportConfigText(v6, "v6.conf"); err != nil {
		t.Fatal(err)
	}
	p, _, _, err := a.buildPlan()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(p.Routes, "::/0") || slices.Contains(p.Addresses, blackholeV6) {
		t.Fatalf("routes %v addresses %v", p.Routes, p.Addresses)
	}
}

// A .vpn key file goes through the key path, not the config parser.
func TestImportVPNFile(t *testing.T) {
	a, _ := testApp(t, 34, func(*store.Settings) {})
	err := a.ImportConfigText("vpn://AAAA", "x.vpn")
	if err == nil || strings.Contains(err.Error(), "конфиг AmneziaWG") {
		t.Fatalf("a vpn:// file was parsed as a config: %v", err)
	}
}

// An IPv6 address with only partial IPv6 routes must still keep all IPv6 in the tunnel.
func TestPlanPartialIPv6(t *testing.T) {
	a, _ := testApp(t, 34, func(s *store.Settings) { s.BlockIPv6 = true })
	v6 := strings.Replace(strings.Replace(testConf, "Address = 10.8.1.23/32", "Address = 10.8.1.23/32, fd00:8::23/128", 1),
		"AllowedIPs = 0.0.0.0/0", "AllowedIPs = 0.0.0.0/0, fd00:8::/64", 1)
	if err := a.ImportConfigText(v6, "v6.conf"); err != nil {
		t.Fatal(err)
	}
	p, _, _, err := a.buildPlan()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(p.Routes, "::/0") || !slices.Contains(p.Routes, "fd00:8::/64") {
		t.Fatalf("routes %v", p.Routes)
	}
}
