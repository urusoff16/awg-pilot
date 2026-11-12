// Package xray runs proxy servers from VPN subscriptions (VLESS, Trojan, Shadowsocks, VMess) with
// Xray-core: it turns share links into Xray outbounds, measures servers and carries the packets of
// the VPN adapter through the chosen server with Xray's own TCP/IP stack.
package xray

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Link is a parsed share link.
type Link struct {
	Proto string // vless, trojan, ss, vmess
	Name  string
	Host  string
	Port  int

	outbound map[string]any // Xray outbound without "tag"; its address is Host
}

// Protocols lists the link schemes ParseLink understands.
var Protocols = []string{"vless", "trojan", "ss", "vmess"}

// ParseLink reads a vless://, trojan://, ss:// or vmess:// link.
func ParseLink(raw string) (*Link, error) {
	raw = strings.TrimSpace(raw)
	scheme, _, ok := strings.Cut(raw, "://")
	if !ok {
		return nil, errors.New("это не ссылка на сервер")
	}
	switch strings.ToLower(scheme) {
	case "vless":
		return parseVLESS(raw)
	case "trojan":
		return parseTrojan(raw)
	case "ss":
		return parseSS(raw)
	case "vmess":
		return parseVMess(raw)
	case "hysteria2", "hy2", "tuic", "wireguard", "socks", "http", "https":
		return nil, fmt.Errorf("протокол %s пока не поддерживается", scheme)
	}
	return nil, fmt.Errorf("неизвестный протокол %q", scheme)
}

// Outbound returns the Xray outbound for the link with the given tag. address, when not empty,
// replaces the server's host name (an IP resolved in advance), while TLS keeps the name as SNI.
func (l *Link) Outbound(tag, address string) map[string]any {
	ob := deepCopy(l.outbound)
	ob["tag"] = tag
	if address != "" {
		if s, ok := ob["settings"].(map[string]any); ok {
			s["address"] = address
		}
	}
	return ob
}

// Endpoint is "host:port" of the server.
func (l *Link) Endpoint() string { return net.JoinHostPort(l.Host, strconv.Itoa(l.Port)) }

func deepCopy(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	json.Unmarshal(b, &out)
	return out
}

// splitURL parses scheme://userinfo@host:port?query#name.
func splitURL(raw string) (*url.URL, string, int, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, "", 0, fmt.Errorf("ссылка повреждена: %w", err)
	}
	host := u.Hostname()
	if host == "" {
		return nil, "", 0, errors.New("в ссылке нет адреса сервера")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port <= 0 || port > 65535 {
		return nil, "", 0, errors.New("в ссылке нет порта сервера")
	}
	return u, host, port, nil
}

func fragmentName(u *url.URL) string {
	if u.Fragment != "" {
		return strings.TrimSpace(u.Fragment)
	}
	return ""
}

// stream builds streamSettings from the common query parameters of vless:// and trojan:// links
// (the format of Xray's share-link proposal, also used by v2rayN, Happ and Remnawave).
func stream(q url.Values, host string, defaultSecurity string) (map[string]any, error) {
	network := strings.ToLower(q.Get("type"))
	switch network {
	case "", "tcp", "raw":
		network = "raw"
	case "splithttp":
		network = "xhttp"
	case "ws", "grpc", "httpupgrade", "xhttp", "kcp", "mkcp":
		if network == "mkcp" {
			network = "kcp"
		}
	case "h2", "http", "quic":
		return nil, fmt.Errorf("транспорт %s больше не поддерживается в Xray", network)
	default:
		return nil, fmt.Errorf("неизвестный транспорт %q", network)
	}
	security := strings.ToLower(q.Get("security"))
	if security == "" {
		security = defaultSecurity
	}
	ss := map[string]any{"network": network, "security": security}

	sni := q.Get("sni")
	if sni == "" {
		sni = q.Get("peer")
	}
	hostHeader := q.Get("host")
	if sni == "" {
		if hostHeader != "" {
			sni = hostHeader
		} else if net.ParseIP(host) == nil {
			sni = host
		}
	}
	fp := q.Get("fp")
	switch security {
	case "tls":
		tls := map[string]any{}
		if sni != "" {
			tls["serverName"] = sni
		}
		if fp != "" {
			tls["fingerprint"] = fp
		}
		if alpn := q.Get("alpn"); alpn != "" {
			tls["alpn"] = strings.Split(alpn, ",")
		}
		if v := q.Get("allowInsecure"); v == "1" || strings.EqualFold(v, "true") {
			tls["allowInsecure"] = true
		}
		if v := q.Get("insecure"); v == "1" || strings.EqualFold(v, "true") {
			tls["allowInsecure"] = true
		}
		if ech := q.Get("ech"); ech != "" {
			tls["echConfigList"] = ech
		}
		if pcs := q.Get("pcs"); pcs != "" {
			tls["pinnedPeerCertSha256"] = pcs
		}
		ss["tlsSettings"] = tls
	case "reality":
		pbk := q.Get("pbk")
		if pbk == "" {
			return nil, errors.New("в ссылке Reality нет открытого ключа (pbk)")
		}
		r := map[string]any{"publicKey": pbk, "serverName": sni}
		if fp == "" {
			fp = "chrome" // Reality needs a browser fingerprint
		}
		r["fingerprint"] = fp
		if sid := q.Get("sid"); sid != "" {
			r["shortId"] = sid
		}
		if spx := q.Get("spx"); spx != "" {
			r["spiderX"] = spx
		}
		if pqv := q.Get("pqv"); pqv != "" {
			r["mldsa65Verify"] = pqv
		}
		ss["realitySettings"] = r
	case "none", "":
		ss["security"] = "none"
	default:
		return nil, fmt.Errorf("неизвестный вид защиты %q", security)
	}

	path := q.Get("path")
	switch network {
	case "raw":
		if strings.EqualFold(q.Get("headerType"), "http") {
			req := map[string]any{}
			if path != "" {
				req["path"] = strings.Split(path, ",")
			}
			if hostHeader != "" {
				req["headers"] = map[string]any{"Host": strings.Split(hostHeader, ",")}
			}
			ss["rawSettings"] = map[string]any{"header": map[string]any{"type": "http", "request": req}}
		}
	case "ws":
		ws := map[string]any{"path": path}
		if hostHeader != "" {
			ws["host"] = hostHeader
		}
		ss["wsSettings"] = ws
	case "httpupgrade":
		hu := map[string]any{"path": path}
		if hostHeader != "" {
			hu["host"] = hostHeader
		}
		ss["httpupgradeSettings"] = hu
	case "grpc":
		g := map[string]any{"serviceName": q.Get("serviceName")}
		if a := q.Get("authority"); a != "" {
			g["authority"] = a
		}
		if strings.EqualFold(q.Get("mode"), "multi") {
			g["multiMode"] = true
		}
		ss["grpcSettings"] = g
	case "xhttp":
		x := map[string]any{"path": path}
		if hostHeader != "" {
			x["host"] = hostHeader
		}
		if m := q.Get("mode"); m != "" {
			x["mode"] = m
		}
		if extra := q.Get("extra"); extra != "" {
			var e any
			if err := json.Unmarshal([]byte(extra), &e); err != nil {
				return nil, fmt.Errorf("параметр extra в ссылке повреждён: %w", err)
			}
			x["extra"] = e
		}
		ss["xhttpSettings"] = x
	case "kcp":
		k := map[string]any{}
		if seed := q.Get("seed"); seed != "" {
			k["seed"] = seed
		}
		ss["kcpSettings"] = k
	}

	// fm is Xray's finalmask (e.g. fragmenting the TLS ClientHello), given as JSON.
	if fm := q.Get("fm"); fm != "" {
		var m any
		if err := json.Unmarshal([]byte(fm), &m); err != nil {
			return nil, fmt.Errorf("параметр fm в ссылке повреждён: %w", err)
		}
		ss["finalmask"] = m
	}
	return ss, nil
}

func parseVLESS(raw string) (*Link, error) {
	u, host, port, err := splitURL(raw)
	if err != nil {
		return nil, err
	}
	id := u.User.Username()
	if id == "" {
		return nil, errors.New("в ссылке VLESS нет идентификатора пользователя")
	}
	q := u.Query()
	enc := q.Get("encryption")
	if enc == "" {
		enc = "none"
	}
	settings := map[string]any{"address": host, "port": port, "id": id, "encryption": enc}
	if flow := q.Get("flow"); flow != "" {
		settings["flow"] = flow
	}
	ss, err := stream(q, host, "none")
	if err != nil {
		return nil, err
	}
	return &Link{Proto: "vless", Name: fragmentName(u), Host: host, Port: port,
		outbound: map[string]any{"protocol": "vless", "settings": settings, "streamSettings": ss}}, nil
}

func parseTrojan(raw string) (*Link, error) {
	u, host, port, err := splitURL(raw)
	if err != nil {
		return nil, err
	}
	pass := u.User.Username()
	if pass == "" {
		return nil, errors.New("в ссылке Trojan нет пароля")
	}
	q := u.Query()
	settings := map[string]any{"address": host, "port": port, "password": pass}
	ss, err := stream(q, host, "tls")
	if err != nil {
		return nil, err
	}
	return &Link{Proto: "trojan", Name: fragmentName(u), Host: host, Port: port,
		outbound: map[string]any{"protocol": "trojan", "settings": settings, "streamSettings": ss}}, nil
}

// decodeB64 accepts standard and URL-safe base64, padded or not.
func decodeB64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, "=")
	for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("не base64")
}

// parseSS reads SIP002 (ss://base64(method:password)@host:port#name, or the userinfo in clear
// for 2022 ciphers) and the legacy ss://base64(method:password@host:port)#name.
func parseSS(raw string) (*Link, error) {
	rest := raw[len("ss://"):]
	name := ""
	if i := strings.IndexByte(rest, '#'); i >= 0 {
		name, _ = url.PathUnescape(rest[i+1:])
		rest = rest[:i]
	}
	if !strings.Contains(rest, "@") {
		dec, err := decodeB64(strings.SplitN(rest, "?", 2)[0])
		if err != nil {
			return nil, errors.New("ссылка Shadowsocks повреждена")
		}
		rest = string(dec)
	}
	u, err := url.Parse("ss://" + rest)
	if err != nil {
		return nil, fmt.Errorf("ссылка Shadowsocks повреждена: %w", err)
	}
	host := u.Hostname()
	port, perr := strconv.Atoi(u.Port())
	if host == "" || perr != nil {
		return nil, errors.New("в ссылке Shadowsocks нет адреса или порта")
	}
	var method, pass string
	if p, ok := u.User.Password(); ok {
		method, pass = u.User.Username(), p
	} else if dec, err := decodeB64(u.User.Username()); err == nil {
		method, pass, _ = strings.Cut(string(dec), ":")
	} else {
		method, pass, _ = strings.Cut(u.User.Username(), ":")
	}
	if method == "" || pass == "" {
		return nil, errors.New("в ссылке Shadowsocks нет шифра или пароля")
	}
	if plugin := u.Query().Get("plugin"); plugin != "" {
		return nil, errors.New("плагины Shadowsocks не поддерживаются")
	}
	settings := map[string]any{"address": host, "port": port, "method": method, "password": pass}
	return &Link{Proto: "ss", Name: strings.TrimSpace(name), Host: host, Port: port,
		outbound: map[string]any{"protocol": "shadowsocks", "settings": settings, "streamSettings": map[string]any{"network": "raw"}}}, nil
}

// parseVMess reads the v2rayN format: vmess://base64(JSON).
func parseVMess(raw string) (*Link, error) {
	dec, err := decodeB64(raw[len("vmess://"):])
	if err != nil {
		return nil, errors.New("ссылка VMess повреждена")
	}
	var v map[string]any
	if err := json.Unmarshal(dec, &v); err != nil {
		return nil, errors.New("ссылка VMess повреждена")
	}
	str := func(k string) string {
		switch x := v[k].(type) {
		case string:
			return x
		case float64:
			return strconv.FormatFloat(x, 'f', -1, 64)
		}
		return ""
	}
	host := str("add")
	port, perr := strconv.Atoi(str("port"))
	if host == "" || perr != nil || str("id") == "" {
		return nil, errors.New("в ссылке VMess нет адреса, порта или идентификатора")
	}
	q := url.Values{}
	q.Set("type", str("net"))
	if str("type") != "" && str("type") != "none" {
		q.Set("headerType", str("type"))
	}
	for _, k := range []string{"host", "path", "sni", "alpn", "fp"} {
		if s := str(k); s != "" {
			q.Set(k, s)
		}
	}
	if q.Get("type") == "grpc" {
		q.Set("serviceName", str("path"))
	}
	q.Set("security", str("tls"))
	if q.Get("security") == "" {
		q.Set("security", "none")
	}
	ss, err := stream(q, host, "none")
	if err != nil {
		return nil, err
	}
	scy := str("scy")
	if scy == "" {
		scy = "auto"
	}
	settings := map[string]any{"address": host, "port": port, "id": str("id"), "security": scy}
	return &Link{Proto: "vmess", Name: strings.TrimSpace(str("ps")), Host: host, Port: port,
		outbound: map[string]any{"protocol": "vmess", "settings": settings, "streamSettings": ss}}, nil
}
