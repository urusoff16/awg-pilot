// Package awgconf parses AmneziaWG wg-quick configs without OS dependencies (used on Android;
// the Windows build parses with amneziawg-windows/conf) and renders them as UAPI for
// amneziawg-go.
package awgconf

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// Config is one [Interface] with exactly one [Peer].
type Config struct {
	PrivateKey [32]byte
	Addresses  []netip.Prefix
	DNS        []netip.Addr
	MTU        int
	// Obfuscation holds the AmneziaWG interface keys (jc, jmin, jmax, s1..s4, h1..h4, i1..i5)
	// in the order they appeared, with lowercase names as UAPI expects.
	Obfuscation [][2]string
	Peer        Peer
}

type Peer struct {
	PublicKey    [32]byte
	PresharedKey [32]byte
	HasPSK       bool
	AllowedIPs   []netip.Prefix
	Endpoint     string // host:port as written
	Keepalive    string // seconds or a range ("25-35"); "0" = off
}

var obfKeys = map[string]bool{"jc": true, "jmin": true, "jmax": true, "s1": true, "s2": true, "s3": true, "s4": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "i1": true, "i2": true, "i3": true, "i4": true, "i5": true}

// extKeys are the AmneziaWG 3.x [Interface] keys (lowercase) and their UAPI names.
var extKeys = map[string]string{
	"headerprotectionkey":    "header_protection_key",
	"contentpaddingaddition": "content_padding_addition",
	"rekeyaftertime":         "rekey_after_time",
	"rekeytimeout":           "rekey_timeout",
	"rejectaftertime":        "reject_after_time",
	"keepalivetimeout":       "keepalive_timeout",
	"maxhandshakeattempts":   "max_handshake_attempts",
	"randomtrailers":         "random_trailers",
	"disablecookies":         "disable_cookies",
}

// ExtValue converts a 3.x config value to its UAPI form: the header protection key goes from
// base64 to hex, on/off switches become true/false, ranges ("100-120") stay as written.
func ExtValue(uapiKey, v string) (string, error) {
	switch uapiKey {
	case "header_protection_key":
		k, err := parseKey(v)
		if err != nil {
			return "", err
		}
		return hex.EncodeToString(k[:]), nil
	case "random_trailers", "disable_cookies":
		switch strings.ToLower(v) {
		case "on", "true", "yes", "1":
			return "true", nil
		case "off", "false", "no", "0", "":
			return "false", nil
		}
		return "", fmt.Errorf("ожидается on или off, получено %q", v)
	}
	return v, nil
}

// Extended holds what SplitExtended took out of a config.
type Extended struct {
	Device    []string // UAPI lines for the device, before any peer
	Keepalive string   // the peer's PersistentKeepalive when it is a range ("25-35"), else ""
}

const bom = string(rune(0xfeff))

// SplitExtended removes the AmneziaWG 3.x keys from a config, so parsers that only know 2.0
// (amneziawg-windows/conf) accept the rest, and returns them in UAPI form.
func SplitExtended(raw string) (string, Extended, error) {
	var keep []string
	var ext Extended
	section := ""
	for n, line := range strings.Split(strings.ReplaceAll(raw, "\r", ""), "\n") {
		t := strings.TrimSpace(strings.TrimPrefix(line, bom))
		if strings.HasPrefix(t, "[") {
			section = strings.ToLower(strings.Trim(t, "[] "))
		}
		k, v, ok := strings.Cut(t, "=")
		if ok && !strings.HasPrefix(t, "#") {
			k = strings.ToLower(strings.TrimSpace(k))
			if i := strings.IndexByte(v, '#'); i >= 0 {
				v = v[:i]
			}
			v = strings.TrimSpace(v)
			switch {
			case section == "interface" && extKeys[k] != "":
				uv, err := ExtValue(extKeys[k], v)
				if err != nil {
					return "", Extended{}, fmt.Errorf("строка %d (%s): %w", n+1, k, err)
				}
				ext.Device = append(ext.Device, extKeys[k]+"="+uv)
				continue
			case section == "peer" && k == "persistentkeepalive" && strings.Contains(v, "-"):
				kv, err := keepalive(v)
				if err != nil {
					return "", Extended{}, fmt.Errorf("строка %d (%s): %w", n+1, k, err)
				}
				ext.Keepalive = kv
				continue
			}
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n"), ext, nil
}

// keepalive validates PersistentKeepalive: seconds, a range of seconds ("25-35", AmneziaWG 3.x)
// or "off". It returns the UAPI value.
func keepalive(v string) (string, error) {
	v = strings.ReplaceAll(strings.TrimSpace(v), " ", "")
	if v == "" || strings.EqualFold(v, "off") {
		return "0", nil
	}
	lo, hi, isRange := strings.Cut(v, "-")
	parts := []string{lo}
	if isRange {
		parts = append(parts, hi)
	}
	for _, part := range parts {
		if n, err := strconv.Atoi(part); err != nil || n < 0 || n > 65535 {
			return "", fmt.Errorf("неверный интервал %q", v)
		}
	}
	return v, nil
}

// Parse reads a wg-quick config. Unknown keys and wg-quick script hooks are ignored.
func Parse(raw string) (*Config, error) {
	c := &Config{}
	section, peers := "", 0
	var haveKey, havePub bool
	for n, line := range strings.Split(strings.ReplaceAll(raw, "\r", ""), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = strings.ToLower(strings.Trim(line, "[] "))
			if section == "peer" {
				peers++
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("строка %d: ожидается «ключ = значение»", n+1)
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		var err error
		switch section {
		case "interface":
			switch {
			case k == "privatekey":
				c.PrivateKey, err = parseKey(v)
				haveKey = err == nil
			case k == "address":
				c.Addresses, err = parsePrefixes(c.Addresses, v, true)
			case k == "dns":
				for _, s := range splitList(v) {
					if ad, e := netip.ParseAddr(s); e == nil {
						c.DNS = append(c.DNS, ad)
					} // search domains are not supported on Android; skip them
				}
			case k == "mtu":
				c.MTU, err = strconv.Atoi(v)
			case obfKeys[k]:
				c.Obfuscation = append(c.Obfuscation, [2]string{k, v})
			case extKeys[k] != "":
				var uv string
				if uv, err = ExtValue(extKeys[k], v); err == nil {
					c.Obfuscation = append(c.Obfuscation, [2]string{extKeys[k], uv})
				}
			}
		case "peer":
			if peers > 1 {
				continue
			}
			switch k {
			case "publickey":
				c.Peer.PublicKey, err = parseKey(v)
				havePub = err == nil
			case "presharedkey":
				c.Peer.PresharedKey, err = parseKey(v)
				c.Peer.HasPSK = err == nil
			case "allowedips":
				c.Peer.AllowedIPs, err = parsePrefixes(c.Peer.AllowedIPs, v, false)
			case "endpoint":
				c.Peer.Endpoint = v
			case "persistentkeepalive":
				c.Peer.Keepalive, err = keepalive(v)
			}
		default:
			return nil, fmt.Errorf("строка %d: ключ вне разделов [Interface]/[Peer]", n+1)
		}
		if err != nil {
			return nil, fmt.Errorf("строка %d (%s): %w", n+1, k, err)
		}
	}
	switch {
	case !haveKey:
		return nil, errors.New("нет PrivateKey в разделе [Interface]")
	case peers != 1:
		return nil, fmt.Errorf("в конфиге должен быть ровно один раздел [Peer], найдено %d", peers)
	case !havePub:
		return nil, errors.New("нет PublicKey в разделе [Peer]")
	case c.Peer.Endpoint == "":
		return nil, errors.New("нет Endpoint в разделе [Peer]")
	case len(c.Addresses) == 0:
		return nil, errors.New("нет Address в разделе [Interface]")
	}
	if c.MTU != 0 && (c.MTU < 576 || c.MTU > 9000) {
		return nil, fmt.Errorf("неверный MTU %d", c.MTU)
	}
	return c, nil
}

func parseKey(s string) ([32]byte, error) {
	var k [32]byte
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != 32 {
		return k, errors.New("неверный ключ")
	}
	copy(k[:], b)
	return k, nil
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// parsePrefixes accepts "a.b.c.d/n" and bare addresses (as /32 or /128). keepHost keeps the
// host bits (interface addresses); otherwise prefixes are masked.
func parsePrefixes(dst []netip.Prefix, v string, keepHost bool) ([]netip.Prefix, error) {
	for _, s := range splitList(v) {
		var p netip.Prefix
		var err error
		if strings.Contains(s, "/") {
			p, err = netip.ParsePrefix(s)
		} else {
			var a netip.Addr
			a, err = netip.ParseAddr(s)
			p = netip.PrefixFrom(a, a.BitLen())
		}
		if err != nil {
			return nil, fmt.Errorf("неверный адрес %q", s)
		}
		if !keepHost {
			p = p.Masked()
		}
		dst = append(dst, p)
	}
	return dst, nil
}

// UAPI renders the device configuration. endpoint is the peer endpoint with the host already
// resolved to an IP; allowed replaces the peer's AllowedIPs when non-nil.
func (c *Config) UAPI(endpoint string, allowed []netip.Prefix) string {
	var b strings.Builder
	fmt.Fprintf(&b, "private_key=%s\n", hex.EncodeToString(c.PrivateKey[:]))
	for _, kv := range c.Obfuscation {
		fmt.Fprintf(&b, "%s=%s\n", kv[0], kv[1])
	}
	b.WriteString("replace_peers=true\n")
	fmt.Fprintf(&b, "public_key=%s\n", hex.EncodeToString(c.Peer.PublicKey[:]))
	if c.Peer.HasPSK {
		fmt.Fprintf(&b, "preshared_key=%s\n", hex.EncodeToString(c.Peer.PresharedKey[:]))
	}
	fmt.Fprintf(&b, "endpoint=%s\n", endpoint)
	ka := c.Peer.Keepalive
	if ka == "" {
		ka = "0"
	}
	fmt.Fprintf(&b, "persistent_keepalive_interval=%s\n", ka)
	b.WriteString("replace_allowed_ips=true\n")
	if allowed == nil {
		allowed = c.Peer.AllowedIPs
	}
	for _, p := range allowed {
		fmt.Fprintf(&b, "allowed_ip=%s\n", p)
	}
	return b.String()
}

// Summary is a secret-free one-line description of the obfuscation parameters.
func (c *Config) Summary() string {
	get := func(k string) string {
		for _, kv := range c.Obfuscation {
			if kv[0] == k {
				return kv[1]
			}
		}
		return "0"
	}
	return fmt.Sprintf("Jc=%s Jmin=%s Jmax=%s S1..S4=%s/%s/%s/%s", get("jc"), get("jmin"), get("jmax"), get("s1"), get("s2"), get("s3"), get("s4"))
}
