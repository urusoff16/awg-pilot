package tunnel

import (
	"strings"
	"testing"
)

// The Windows parser (amneziawg-windows, 2.0 only) must accept 3.1 configs once their extra
// keys are split off.
func TestParseConfig31(t *testing.T) {
	raw := strings.Join([]string{"[Interface]", "PrivateKey = YAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk=",
		"Address = 10.8.1.3/32", "Jc = 6", "S1 = 12", "S2 = 12", "H1 = 1", "H2 = 2", "H3 = 3", "H4 = 4",
		"HeaderProtectionKey = 5tVmWdtdleeVZQK8lf0vR9HkBbdOtzE7y3JF0zB6Eiw=", "RandomTrailers = on", "RekeyAfterTime = 100-120",
		"", "[Peer]", "PublicKey = xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg=", "AllowedIPs = 0.0.0.0/0", "Endpoint = 198.51.100.7:36507"}, "\n")
	c, err := ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if c.Interface.JunkPacketCount != 6 || len(c.Peers) != 1 {
		t.Fatalf("unexpected config: %+v", c.Interface)
	}
}
