package tunnel

import (
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/amnezia-vpn/amneziawg-go/v3/conn"
	"github.com/amnezia-vpn/amneziawg-go/v3/device"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun/tuntest"
)

// TestRealConfigAccepted feeds a real provider config into an amneziawg-go device backed by an
// in-memory TUN (no adapter, no network). Opt in with AWGPILOT_REAL_CONF=<path to .conf>.
// Only key names are logged, never values.
func TestRealConfigAccepted(t *testing.T) {
	path := os.Getenv("AWGPILOT_REAL_CONF")
	if path == "" {
		t.Skip("set AWGPILOT_REAL_CONF to run")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfig(string(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	uapi, dnsErr := cfg.ToUAPI()
	if dnsErr != nil {
		t.Fatalf("uapi: %v", dnsErr)
	}
	var keys []string
	for _, line := range strings.Split(uapi, "\n") {
		if k, _, ok := strings.Cut(line, "="); ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	t.Logf("uapi keys: %s", strings.Join(keys, " "))
	for _, want := range []string{"s3", "s4", "h1", "h4", "jc"} {
		if !strings.Contains(uapi, want+"=") {
			t.Errorf("uapi lacks %s", want)
		}
	}

	dev := device.NewDevice(tuntest.NewChannelTUN().TUN(), conn.NewDefaultBind(), device.NewLogger(device.LogLevelError, "test: "))
	defer dev.Close()
	if err := dev.IpcSet(uapi); err != nil {
		t.Fatalf("amneziawg-go rejected the config: %v", err)
	}
	got, err := dev.IpcGet()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"s3=", "s4=", "h1=", "public_key=", "allowed_ip=0.0.0.0/0"} {
		if !strings.Contains(got, want) {
			t.Errorf("device state lacks %q", want)
		}
	}
}
