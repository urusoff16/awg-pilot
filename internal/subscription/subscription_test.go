package subscription

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Made-up servers in the shapes real subscriptions use (no real credentials).
var sampleLinks = []string{
	"vless://11111111-2222-3333-4444-555555555555@nl.example.com:8443?encryption=none&flow=xtls-rprx-vision&type=tcp&security=tls&sni=cdn.example.com&fp=firefox&alpn=h2%2Chttp%2F1.1#%F0%9F%87%B3%F0%9F%87%B1%20SMART-%D0%9D%D0%B8%D0%B4%D0%B5%D1%80%D0%BB%D0%B0%D0%BD%D0%B4%D1%8B",
	"vless://11111111-2222-3333-4444-555555555555@198.51.100.7:20443?encryption=none&flow=xtls-rprx-vision&type=tcp&security=reality&sni=example.org&fp=qq&pbk=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA&sid=0123abcd&spx=%2F#%F0%9F%87%B8%F0%9F%87%AA%20%2314%20LT%D0%95%20%D0%A3%D0%BD%D0%B8%D0%B2%D0%B5%D1%80%D1%81%D0%B0%D0%BB%D1%8C%D0%BD%D1%8B%D0%B9",
	"vless://11111111-2222-3333-4444-555555555555@ws.example.net:443?encryption=none&type=ws&path=%2Flive%2Fstream%3Fmodule%3Dvideo&host=ws.example.net&security=tls&sni=ws.example.net&fp=firefox&alpn=h3%2Ch2%2Chttp%2F1.1#%2324%20LTE%20Universal",
	"vless://11111111-2222-3333-4444-555555555555@198.51.100.8:443?encryption=none&flow=xtls-rprx-vision&type=tcp&security=reality&sni=example.org&fp=firefox&pbk=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA&sid=ab&fm=%7B%22tcp%22%3A%5B%7B%22type%22%3A%22fragment%22%2C%22settings%22%3A%7B%22delays%22%3A%5B%220%22%5D%2C%22lengths%22%3A%5B%2210-20%22%5D%2C%22packets%22%3A%22tlshello%22%7D%7D%5D%7D#BRIDGE-Germany",
	"trojan://secret@tr.example.com:443?security=tls&sni=tr.example.com#Trojan",
	"ss://" + base64.RawURLEncoding.EncodeToString([]byte("chacha20-ietf-poly1305:pass")) + "@203.0.113.5:8388#SS",
	"hysteria2://pass@hy.example.com:443#Hy2",
}

func TestParse(t *testing.T) {
	body := base64.StdEncoding.EncodeToString([]byte(strings.Join(sampleLinks, "\n")))
	servers, skipped := Parse([]byte(body))
	if len(servers) != 6 || len(skipped) != 1 {
		t.Fatalf("servers %d skipped %v", len(servers), skipped)
	}
	want := []struct{ name, group, proto string }{
		{"🇳🇱 SMART-Нидерланды", "SMART", "vless"},
		{"🇸🇪 #14 LTЕ Универсальный", "LTE", "vless"}, // Cyrillic Е in the name
		{"#24 LTE Universal", "LTE", "vless"},
		{"BRIDGE-Germany", "BRIDGE", "vless"},
		{"Trojan", "", "trojan"},
		{"SS", "", "ss"},
	}
	for i, w := range want {
		s := servers[i]
		if s.Name != w.name || s.Group != w.group || s.Proto != w.proto {
			t.Errorf("%d: got %q/%q/%q, want %q/%q/%q", i, s.Name, s.Group, s.Proto, w.name, w.group, w.proto)
		}
	}
	// Plain (not base64) bodies too.
	plain, _ := Parse([]byte(strings.Join(sampleLinks[:2], "\r\n")))
	if len(plain) != 2 || plain[0].ID != servers[0].ID {
		t.Fatalf("plain: %+v", plain)
	}
}

func TestIDIgnoresName(t *testing.T) {
	a, _ := Parse([]byte(sampleLinks[0]))
	b, _ := Parse([]byte(strings.Replace(sampleLinks[0], "#%F0", "#X%F0", 1)))
	if a[0].ID != b[0].ID {
		t.Fatal("renaming a server changed its ID")
	}
}

func TestHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("profile-title", "base64:"+base64.StdEncoding.EncodeToString([]byte("🚀 Velvet")))
	h.Set("subscription-userinfo", "upload=0; download=10074835086; total=1099511627776; expire=1796636535")
	h.Set("profile-update-interval", "2")
	h.Set("support-url", "https://t.me/support_bot")
	h.Set("profile-web-page-url", "javascript:alert(1)")
	h.Set("announce", "plain text")
	in := ParseHeaders(h)
	if in.Title != "🚀 Velvet" || in.Download != 10074835086 || in.Total != 1099511627776 || in.Expire != 1796636535 ||
		in.UpdateHours != 2 || in.SupportURL == "" || in.WebPage != "" || in.Announce != "plain text" {
		t.Fatalf("%+v", in)
	}
}

func TestFetch(t *testing.T) {
	var gotHWID, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHWID, gotUA = r.Header.Get("x-hwid"), r.Header.Get("User-Agent")
		w.Header().Set("subscription-userinfo", "upload=1; download=2; total=3; expire=0")
		w.Write([]byte(base64.StdEncoding.EncodeToString([]byte(strings.Join(sampleLinks, "\n")))))
	}))
	defer srv.Close()
	info, servers, err := Fetch(context.Background(), srv.Client(), srv.URL, Headers{UserAgent: "AWGPilot/1", HWID: "abc"})
	if err != nil || len(servers) != 6 || info.Total != 3 || gotHWID != "abc" || gotUA != "AWGPilot/1" {
		t.Fatalf("%v %d %+v %q %q", err, len(servers), info, gotHWID, gotUA)
	}
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<!doctype html><html><body>hi</body></html>"))
	}))
	defer html.Close()
	if _, _, err := Fetch(context.Background(), html.Client(), html.URL, Headers{}); err == nil || !strings.Contains(err.Error(), "веб-страница") {
		t.Fatalf("html page: %v", err)
	}
}

func TestCandidates(t *testing.T) {
	servers, _ := Parse([]byte(strings.Join(sampleLinks, "\n")))
	now := time.Now()
	m := map[string]Measure{
		servers[0].ID: {Ms: 300, At: now}, // SMART
		servers[1].ID: {Ms: -1, At: now},  // LTE, failed
		servers[2].ID: {Ms: 500, At: now}, // LTE
		servers[3].ID: {Ms: 200, At: now}, // BRIDGE
		servers[4].ID: {Ms: 100, At: now}, // Trojan
	}
	ids := func(list []Server) string {
		var out []string
		for _, s := range list {
			out = append(out, s.Name)
		}
		return strings.Join(out, ",")
	}
	if r := DefaultRule(NetMobile, servers); r != "auto:LTE" {
		t.Fatal(r)
	}
	if r := DefaultRule(NetWiFi, servers); r != "auto:!LTE" {
		t.Fatal(r)
	}
	got := ids(Candidates(servers, "auto:LTE", m, now))
	if !strings.HasPrefix(got, "#24 LTE Universal,🇸🇪 #14 LTЕ Универсальный,Trojan,") {
		t.Fatalf("mobile: %s", got)
	}
	got = ids(Candidates(servers, "auto:!LTE", m, now))
	if !strings.HasPrefix(got, "Trojan,BRIDGE-Germany,🇳🇱 SMART-Нидерланды,SS,") {
		t.Fatalf("wifi: %s", got)
	}
	got = ids(Candidates(servers, "server:"+servers[3].ID, m, now))
	if !strings.HasPrefix(got, "BRIDGE-Germany,Trojan,") || len(Candidates(servers, "server:x", m, now)) != len(servers) {
		t.Fatalf("fixed: %s", got)
	}
	if !NeedTest(servers, "auto:!LTE", m, now) { // SS has no measurement
		t.Fatal("SS untested")
	}
	if NeedTest(servers, "auto:LTE", m, now) {
		t.Fatal("LTE servers are measured")
	}
	if g := strings.Join(Groups(servers), ","); g != "SMART,LTE,BRIDGE" {
		t.Fatal(g)
	}
}
