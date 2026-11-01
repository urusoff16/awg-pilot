package awgconf

import (
	"strings"
	"testing"
)

const sample = `[Interface]
PrivateKey = YAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk=
Address = 10.8.1.23/32
DNS = 10.8.1.1, 1.1.1.1
MTU = 1280
Jc = 4
Jmin = 10
Jmax = 50
S1 = 64
S2 = 51
S3 = 17
S4 = 5
H1 = 100000-100200
H2 = 200000
H3 = 300000
H4 = 400000

[Peer]
PublicKey = xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg=
PresharedKey = HIgo9xNzJMWLKASShiTqIybxZ0U3wGLiUeJ1PKf8ykw=
AllowedIPs = 0.0.0.0/0, ::/0
Endpoint = 193.0.2.7:35772
PersistentKeepalive = 25
`

func TestParseAndUAPI(t *testing.T) {
	c, err := Parse("\ufeff" + strings.ReplaceAll(sample, "\n", "\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.MTU != 1280 || len(c.DNS) != 2 || len(c.Addresses) != 1 || len(c.Peer.AllowedIPs) != 2 || c.Peer.Keepalive != "25" || !c.Peer.HasPSK {
		t.Fatalf("unexpected parse result: %+v", c)
	}
	u := c.UAPI("193.0.2.7:35772", nil)
	for _, want := range []string{"jc=4\n", "s3=17\n", "s4=5\n", "h1=100000-100200\n", "replace_peers=true\n",
		"endpoint=193.0.2.7:35772\n", "allowed_ip=0.0.0.0/0\n", "allowed_ip=::/0\n", "persistent_keepalive_interval=25\n"} {
		if !strings.Contains(u, want) {
			t.Errorf("UAPI lacks %q:\n%s", want, u)
		}
	}
	if strings.Index(u, "private_key=") != 0 {
		t.Errorf("private_key must come first")
	}
}

func TestParseErrors(t *testing.T) {
	for name, cfg := range map[string]string{
		"no peer":   "[Interface]\nPrivateKey = YAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk=\nAddress = 10.0.0.2/32\n",
		"two peers": sample + "\n[Peer]\nPublicKey = xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg=\nEndpoint = 1.2.3.4:5\n",
		"bad key":   strings.Replace(sample, "YAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk=", "short", 1),
	} {
		if _, err := Parse(cfg); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// An AmneziaWG 3.1 config as AmneziaVPN issues it (keys are made up).
const sample31 = `[Interface]
PrivateKey = YAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk=
Address = 10.8.1.3/32
DNS = 1.1.1.1
Jc = 6
Jmin = 10
Jmax = 50
S1 = 12
S2 = 12
S3 = 12
S4 = 12
H1 = 1
H2 = 2
H3 = 3
H4 = 4
I1 = <r 2><b 0x8580000100010000>
HeaderProtectionKey = 5tVmWdtdleeVZQK8lf0vR9HkBbdOtzE7y3JF0zB6Eiw=
RekeyAfterTime = 100-120
RekeyTimeout = 3-7
RejectAfterTime = 150-180
KeepaliveTimeout = 5-15
MaxHandshakeAttempts = 15-20
RandomTrailers = on
DisableCookies = on

[Peer]
PublicKey = xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg=
AllowedIPs = 0.0.0.0/0
Endpoint = 198.51.100.7:36507
PersistentKeepalive = 25
`

func TestParse31(t *testing.T) {
	c, err := Parse(sample31)
	if err != nil {
		t.Fatal(err)
	}
	u := c.UAPI("198.51.100.7:36507", nil)
	for _, want := range []string{"header_protection_key=e6d56659db5d95e7956502bc95fd2f47d1e405b74eb7313bcb7245d3307a122c\n",
		"rekey_after_time=100-120\n", "max_handshake_attempts=15-20\n", "random_trailers=true\n", "disable_cookies=true\n", "i1=<r 2><b 0x8580000100010000>\n"} {
		if !strings.Contains(u, want) {
			t.Errorf("UAPI lacks %q:\n%s", want, u)
		}
	}
	if strings.Index(u, "random_trailers") > strings.Index(u, "public_key=") {
		t.Error("device settings must precede the peer")
	}
}

func TestSplitExtended(t *testing.T) {
	rest, ext, err := SplitExtended(strings.Replace(sample31, "PersistentKeepalive = 25", "PersistentKeepalive = 25-35", 1))
	if err != nil {
		t.Fatal(err)
	}
	if ext.Keepalive != "25-35" || strings.Contains(rest, "PersistentKeepalive") {
		t.Fatalf("keepalive range not split off: %q", ext.Keepalive)
	}
	if len(ext.Device) != 8 || strings.Contains(rest, "HeaderProtectionKey") || !strings.Contains(rest, "Jc = 6") || !strings.Contains(rest, "[Peer]") {
		t.Fatalf("ext=%v rest=%s", ext, rest)
	}
	if _, _, err := SplitExtended(strings.Replace(sample31, "RandomTrailers = on", "RandomTrailers = maybe", 1)); err == nil {
		t.Error("a bad switch value was accepted")
	}
}
