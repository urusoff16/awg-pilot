package main

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"awgpilot/internal/geoip"
	"awgpilot/internal/routing"
)

// TestFitBypassRealList runs on the cached Russian list of the Windows app when it exists.
func TestFitBypassRealList(t *testing.T) {
	dir, _ := os.UserConfigDir()
	l, err := geoip.Load(filepath.Join(dir, "AWGPilot", "ru-ipv4.txt"))
	if err != nil {
		t.Skip("no cached list:", err)
	}
	lan := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("192.168.0.0/16")}
	for _, ex := range []bool{true, false} {
		kept, share := fitBypass(lan, l.Prefixes, nil, ex)
		n := len(routing.Subtract(allIPv4, append(kept, lan...)))
		if ex {
			n = len(routing.Merge(append(kept, lan...)))
		}
		t.Logf("exclude=%v: %d of %d blocks, %.1f%% of Russian addresses direct, %d routes", ex, len(kept), len(routing.Merge(l.Prefixes)), share*100, n)
		if n > routeBudget {
			t.Errorf("%d routes exceed the budget", n)
		}
	}
}
