package main

import (
	"io"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/conn"
	"github.com/amnezia-vpn/amneziawg-go/v3/device"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun/netstack"

	"awgpilot/internal/awgconf"
)

// TestLiveConfig connects to a real server with the config in AWGPILOT_LIVE_CONF through a
// user-space network stack (no admin rights, no system tunnel) and fetches a page through it.
func TestLiveConfig(t *testing.T) {
	path := os.Getenv("AWGPILOT_LIVE_CONF")
	if path == "" {
		t.Skip("set AWGPILOT_LIVE_CONF to a .conf file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := awgconf.Parse(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	ep, err := resolveEndpoint(cfg.Peer.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	var local []netip.Addr
	for _, a := range cfg.Addresses {
		local = append(local, a.Addr())
	}
	td, tnet, err := netstack.CreateNetTUN(local, cfg.DNS, 1280)
	if err != nil {
		t.Fatal(err)
	}
	dev := device.NewDevice(td, conn.NewDefaultBind(), device.NewLogger(device.LogLevelError, "awg: "))
	defer dev.Close()
	if err := dev.IpcSet(cfg.UAPI(ep, nil)); err != nil {
		t.Fatal(err)
	}
	if err := dev.Up(); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{DialContext: tnet.DialContext}, Timeout: 30 * time.Second}
	resp, err := client.Get("https://speed.cloudflare.com/cdn-cgi/trace")
	if err != nil {
		t.Fatalf("no traffic through the tunnel: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "loc=") || strings.HasPrefix(l, "warp=") {
			t.Log("through the tunnel:", l)
		}
	}
	st, _ := dev.IpcGet()
	if !strings.Contains(st, "last_handshake_time_sec=") || strings.Contains(st, "last_handshake_time_sec=0\n") {
		t.Fatal("no handshake")
	}
}
