package vpnkey

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
)

// TestRealKey decodes a real key from a file; opt in with AWGPILOT_REAL_KEY_FILE. Logs only masked values.
func TestRealKey(t *testing.T) {
	p := os.Getenv("AWGPILOT_REAL_KEY_FILE")
	if p == "" {
		t.Skip("set AWGPILOT_REAL_KEY_FILE to run")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	k, err := Decode(string(b))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("endpoint=%s key=%s version=%d", k.APIEndpoint, k.Masked(), k.ConfigVersion)
}

func TestRoundTrip(t *testing.T) {
	in := &Key{APIEndpoint: "https://api.example.test/", APIKey: "00000000-1111-2222-3333-444444444444", ConfigVersion: 1, Description: "demo"}
	s, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Decode("  " + s + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if out.APIEndpoint != "https://api.example.test" || out.APIKey != in.APIKey || out.ConfigVersion != 1 {
		t.Fatalf("unexpected decode: %+v", out)
	}
	if got := out.Masked(); got != "0000…4444" {
		t.Fatalf("masked = %q", got)
	}
}

func TestRejects(t *testing.T) {
	for _, s := range []string{"", "vless://abc", "vpn://!!!", "vpn://AAAA"} {
		if _, err := Decode(s); err == nil {
			t.Errorf("Decode(%q) succeeded, want error", s)
		}
	}
	plain, _ := Encode(&Key{APIEndpoint: "http://insecure.test", APIKey: "x"})
	if _, err := Decode(plain); err == nil {
		t.Error("http endpoint accepted")
	}
}

func TestDecodeNative(t *testing.T) {
	enc := func(v any) string {
		plain, _ := json.Marshal(v)
		var buf bytes.Buffer
		n := len(plain)
		buf.Write([]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
		zw := zlib.NewWriter(&buf)
		zw.Write(plain)
		zw.Close()
		return "vpn://" + base64.RawURLEncoding.EncodeToString(buf.Bytes())
	}
	last, _ := json.Marshal(map[string]string{"config": "[Interface]\nDNS = $PRIMARY_DNS, $SECONDARY_DNS\n"})
	key := enc(map[string]any{"description": "NL", "hostName": "198.51.100.7", "dns1": "1.1.1.1", "dns2": "1.0.0.1",
		"containers": []any{map[string]any{"container": "amnezia-awg2", "awg": map[string]any{"last_config": string(last)}},
			map[string]any{"container": "amnezia-xray", "xray": map[string]any{}}}})
	n, err := DecodeNative(key)
	if err != nil {
		t.Fatal(err)
	}
	if n.Name != "NL" || n.Config != "[Interface]\nDNS = 1.1.1.1, 1.0.0.1\n" || len(n.Others) != 1 {
		t.Fatalf("%+v", n)
	}
	if _, err := Decode(key); err == nil {
		t.Error("a self-hosted key passed as a provider key")
	}
	xrayOnly := enc(map[string]any{"hostName": "198.51.100.7", "containers": []any{map[string]any{"container": "amnezia-xray"}}})
	if n, err := DecodeNative(xrayOnly); err != nil || n.Config != "" || n.Others[0] != "amnezia-xray" {
		t.Fatalf("xray-only key: %+v %v", n, err)
	}
}
