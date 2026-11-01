package speedtest

import (
	"context"
	"net"
	"os"
	"testing"
	"time"
)

// TestLive runs a real test over the default route when AWGPILOT_LIVE_SPEED=1.
func TestLive(t *testing.T) {
	if os.Getenv("AWGPILOT_LIVE_SPEED") != "1" {
		t.Skip("set AWGPILOT_LIVE_SPEED=1 for a real speed test")
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	r, err := Run(ctx, d.DialContext, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("download %.1f Mbit/s, upload %.1f Mbit/s, exit country %s", r.DownMbps, r.UpMbps, r.Loc)
	if r.DownMbps <= 0 || r.UpMbps <= 0 {
		t.Fatal("no throughput measured")
	}
}
