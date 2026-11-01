package geoip

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"awgpilot/internal/routing"
)

func fakeRIPE(n int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ps []string
		for i := 0; i < n; i++ {
			ps = append(ps, fmt.Sprintf(`"%d.%d.0.0/16"`, 2+i/256, i%256))
		}
		fmt.Fprintf(w, `{"status":"ok","data":{"resources":{"ipv4":[%s]}}}`, strings.Join(ps, ","))
	}))
}

func TestFetchSaveLoad(t *testing.T) {
	srv := fakeRIPE(1500)
	defer srv.Close()
	l, err := Fetch(context.Background(), srv.URL)
	if err != nil || len(l.Prefixes) != 1500 {
		t.Fatalf("fetch: %v %d", err, len(l.Prefixes))
	}
	path := filepath.Join(t.TempDir(), "ru.txt")
	if err := l.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := Load(path)
	if err != nil || len(back.Prefixes) != 1500 || back.FetchedAt.IsZero() {
		t.Fatalf("load: %v", err)
	}
}

func TestRejectsShortList(t *testing.T) {
	srv := fakeRIPE(10)
	defer srv.Close()
	if _, err := Fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("short list accepted")
	}
}

func TestGetUsesFreshCacheWithoutNetwork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ru.txt")
	var ps []netip.Prefix
	for i := 0; i < 1200; i++ {
		ps = append(ps, netip.PrefixFrom(netip.AddrFrom4([4]byte{5, byte(i / 256), byte(i % 256), 0}), 24))
	}
	(&List{Prefixes: ps, FetchedAt: time.Now()}).Save(path)
	l, err := Get(context.Background(), path) // RipeURL is not contacted for a fresh cache
	if err != nil || len(l.Prefixes) != 1200 {
		t.Fatalf("%v", err)
	}
}

// TestRealRIPE measures the real list; opt in with AWGPILOT_RIPE=1 (needs internet).
func TestRealRIPE(t *testing.T) {
	if os.Getenv("AWGPILOT_RIPE") == "" {
		t.Skip("set AWGPILOT_RIPE=1")
	}
	l, err := Fetch(context.Background(), RipeURL)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	merged := routing.Merge(l.Prefixes)
	routes := routing.Subtract([]netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}, l.Prefixes)
	t.Logf("RIPE RU: %d prefixes, %d after merge, %d tunnel routes, computed in %v", len(l.Prefixes), len(merged), len(routes), time.Since(start))
	for _, probe := range []string{"77.88.55.88", "87.250.250.242", "8.8.8.8", "1.1.1.1"} { // yandex.ru, ya.ru, google, cloudflare
		a := netip.MustParseAddr(probe)
		inTunnel := false
		for _, p := range routes {
			if p.Contains(a) {
				inTunnel = true
				break
			}
		}
		t.Logf("%s → %s", probe, map[bool]string{true: "через VPN", false: "напрямую"}[inTunnel])
	}
}
