package main

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/amnezia-vpn/amneziawg-go/v3/conn"
	"github.com/amnezia-vpn/amneziawg-go/v3/device"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun/tuntest"

	"awgpilot/internal/awgconf"
)

// The real engine must accept every setting the parser emits, for 2.0 and 3.1 configs.
func TestEngineAcceptsConfigs(t *testing.T) {
	v31 := strings.Replace(testConf, "Jc = 4", "Jc = 4\nHeaderProtectionKey = 5tVmWdtdleeVZQK8lf0vR9HkBbdOtzE7y3JF0zB6Eiw=\n"+
		"RekeyAfterTime = 100-120\nRekeyTimeout = 3-7\nRejectAfterTime = 150-180\nKeepaliveTimeout = 5-15\n"+
		"MaxHandshakeAttempts = 15-20\nRandomTrailers = on\nDisableCookies = on\nContentPaddingAddition = 0-16\n"+
		"S1 = 12\nS2 = 12\nS3 = 12\nS4 = 12", 1)
	for name, raw := range map[string]string{"2.0": testConf, "3.1": v31} {
		cfg, err := awgconf.Parse(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		dev := device.NewDevice(tuntest.NewChannelTUN().TUN(), conn.NewDefaultBind(), device.NewLogger(device.LogLevelSilent, ""))
		err = dev.IpcSet(cfg.UAPI("192.0.2.7:35772", []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}))
		dev.Close()
		if err != nil {
			t.Errorf("%s: the engine rejected the config: %v", name, err)
		}
	}
}
