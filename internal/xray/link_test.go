package xray

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func outboundJSON(t *testing.T, link string) map[string]any {
	t.Helper()
	l, err := ParseLink(link)
	if err != nil {
		t.Fatal(err)
	}
	ob := l.Outbound("proxy", "192.0.2.1")
	b, _ := json.Marshal(ob)
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func get(m map[string]any, path string) any {
	var cur any = m
	for _, k := range strings.Split(path, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

func TestVLESSReality(t *testing.T) {
	m := outboundJSON(t, "vless://uuid-1@host.example.com:443?encryption=none&flow=xtls-rprx-vision&type=tcp&security=reality&sni=deepl.com&fp=qq&pbk=KEY&sid=ab12&spx=%2F#name")
	checks := map[string]any{
		"protocol":                "vless",
		"settings.address":        "192.0.2.1", // resolved address replaces the name
		"settings.id":             "uuid-1",
		"settings.flow":           "xtls-rprx-vision",
		"streamSettings.network":  "raw",
		"streamSettings.security": "reality",
		"streamSettings.realitySettings.serverName":  "deepl.com",
		"streamSettings.realitySettings.publicKey":   "KEY",
		"streamSettings.realitySettings.shortId":     "ab12",
		"streamSettings.realitySettings.fingerprint": "qq",
		"streamSettings.realitySettings.spiderX":     "/",
	}
	for p, want := range checks {
		if got := get(m, p); got != want {
			t.Errorf("%s = %v, want %v", p, got, want)
		}
	}
}

func TestVLESSWebSocketAndFinalmask(t *testing.T) {
	m := outboundJSON(t, "vless://u@ws.example.net:443?type=ws&path=%2Flive%2Fstream%3Fa%3Db&host=cdn.example.net&security=tls&fp=firefox&alpn=h2%2Chttp%2F1.1&fm=%7B%22tcp%22%3A%5B%7B%22type%22%3A%22fragment%22%7D%5D%7D")
	if get(m, "streamSettings.wsSettings.path") != "/live/stream?a=b" || get(m, "streamSettings.wsSettings.host") != "cdn.example.net" {
		t.Fatalf("ws: %v", get(m, "streamSettings.wsSettings"))
	}
	if get(m, "streamSettings.tlsSettings.serverName") != "cdn.example.net" { // SNI from the Host when sni is absent
		t.Fatalf("sni: %v", get(m, "streamSettings.tlsSettings"))
	}
	if get(m, "streamSettings.finalmask") == nil {
		t.Fatal("finalmask lost")
	}
}

func TestTrojanSSVMess(t *testing.T) {
	m := outboundJSON(t, "trojan://pw@t.example.com:443#t")
	if get(m, "streamSettings.security") != "tls" || get(m, "settings.password") != "pw" {
		t.Fatalf("trojan: %v", m)
	}
	ss := "ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:p@ss")) + "@203.0.113.1:8388#s"
	m = outboundJSON(t, ss)
	if get(m, "settings.method") != "aes-256-gcm" || get(m, "settings.password") != "p@ss" {
		t.Fatalf("ss: %v", m)
	}
	vm, _ := json.Marshal(map[string]any{"v": "2", "ps": "vm", "add": "v.example.com", "port": "443", "id": "u", "net": "ws", "path": "/p", "host": "h", "tls": "tls"})
	m = outboundJSON(t, "vmess://"+base64.StdEncoding.EncodeToString(vm))
	if get(m, "protocol") != "vmess" || get(m, "streamSettings.wsSettings.path") != "/p" || get(m, "streamSettings.security") != "tls" {
		t.Fatalf("vmess: %v", m)
	}
}

// Every link must also be accepted by Xray itself.
func TestXrayAcceptsOutbounds(t *testing.T) {
	links := []string{
		"vless://11111111-2222-3333-4444-555555555555@198.51.100.8:443?encryption=none&flow=xtls-rprx-vision&type=tcp&security=reality&sni=example.org&fp=firefox&pbk=Z84J2IelR9ch3k8VtlVhhs5ycBUlXA7wHBWcBrjqnAw&sid=ab&spx=%2F&fm=%7B%22tcp%22%3A%5B%7B%22type%22%3A%22fragment%22%2C%22settings%22%3A%7B%22delays%22%3A%5B%220%22%5D%2C%22lengths%22%3A%5B%2210-20%22%5D%2C%22packets%22%3A%22tlshello%22%7D%7D%5D%7D#x",
		"vless://11111111-2222-3333-4444-555555555555@ws.example.net:443?encryption=none&type=ws&path=%2Fp&host=ws.example.net&security=tls&sni=ws.example.net&fp=firefox&alpn=h3%2Ch2%2Chttp%2F1.1#x",
		"trojan://pw@t.example.com:443?type=grpc&serviceName=svc#t",
		"ss://" + base64.RawURLEncoding.EncodeToString([]byte("chacha20-ietf-poly1305:pass")) + "@203.0.113.5:8388#SS",
	}
	for _, l := range links {
		pl, err := ParseLink(l)
		if err != nil {
			t.Fatal(err)
		}
		inst, err := Start(pl, "")
		if err != nil {
			t.Fatalf("%s: %v", pl.Proto, err)
		}
		inst.Close()
	}
}
