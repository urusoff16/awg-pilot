// Package routing computes the AllowedIPs set for split tunneling.
package routing

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

type Mode string

const (
	ModeFull    Mode = "full"    // everything through the tunnel
	ModeExclude Mode = "exclude" // everything except Entries
	ModeInclude Mode = "include" // only Entries
)

// Settings are user split-tunneling preferences. Entries are IPs, CIDRs or domain names.
type Settings struct {
	Mode      Mode     `json:"mode"`
	Entries   []string `json:"entries"`
	BypassLAN bool     `json:"bypass_lan"`
	// BypassRussia sends Russian networks (RIPE NCC country list) directly instead of via the VPN.
	BypassRussia bool `json:"bypass_russia"`
}

// LAN and other non-routable IPv4 ranges kept off the tunnel when BypassLAN is set.
var lanPrefixes = mustPrefixes("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "224.0.0.0/4", "255.255.255.255/32")

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// Resolver resolves domain entries; net.DefaultResolver satisfies it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Result is the computed routing plan.
type Result struct {
	AllowedIPs []netip.Prefix
	// FullTunnel is true when the default route goes through the tunnel; only then is a kill switch meaningful.
	FullTunnel bool
	Warnings   []string
}

// Compute builds AllowedIPs. bypass (for example Russian networks) is sent directly in the full
// and exclude modes. The tunnel's DNS servers always stay inside the tunnel, even when they fall
// into a bypassed range such as 172.16.0.0/12. Only IPv4 is routed; IPv6 is left to the OS.
func Compute(ctx context.Context, s Settings, tunnelDNS []netip.Addr, r Resolver, bypass []netip.Prefix) (Result, error) {
	var res Result
	entries, warns := resolveEntries(ctx, s.Entries, r)
	res.Warnings = warns
	var dns []netip.Prefix
	for _, d := range tunnelDNS {
		if d.Is4() {
			dns = append(dns, netip.PrefixFrom(d, 32))
		}
	}
	all := []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}

	switch s.Mode {
	case ModeFull, "", ModeExclude:
		var excl []netip.Prefix
		if s.Mode == ModeExclude {
			excl = append(excl, entries...)
		}
		if s.BypassLAN {
			excl = append(excl, lanPrefixes...)
		}
		excl = append(excl, bypass...)
		if len(excl) == 0 {
			res.AllowedIPs = all
			res.FullTunnel = true
			break
		}
		res.AllowedIPs = Merge(append(Subtract(all, excl), dns...))
	case ModeInclude:
		incl := append(append([]netip.Prefix{}, entries...), dns...)
		if len(entries) == 0 {
			return res, fmt.Errorf("в режиме «только список» нужен хотя бы один адрес или домен")
		}
		res.AllowedIPs = Merge(incl)
	default:
		return res, fmt.Errorf("неизвестный режим раздельного туннеля %q", s.Mode)
	}
	if len(res.AllowedIPs) == 0 {
		return res, fmt.Errorf("список исключений покрывает все адреса — через VPN ничего не пойдёт")
	}
	return res, nil
}

func resolveEntries(ctx context.Context, entries []string, r Resolver) ([]netip.Prefix, []string) {
	var out []netip.Prefix
	var warns []string
	for _, raw := range entries {
		e := strings.TrimSpace(raw)
		if e == "" || strings.HasPrefix(e, "#") {
			continue
		}
		if p, err := netip.ParsePrefix(e); err == nil {
			if p.Addr().Is4() {
				out = append(out, p.Masked())
			} else {
				warns = append(warns, fmt.Sprintf("%s: адреса IPv6 пропущены", e))
			}
			continue
		}
		if a, err := netip.ParseAddr(e); err == nil {
			if a.Is4() {
				out = append(out, netip.PrefixFrom(a, 32))
			} else {
				warns = append(warns, fmt.Sprintf("%s: адреса IPv6 пропущены", e))
			}
			continue
		}
		host := strings.TrimPrefix(strings.TrimPrefix(e, "*."), ".")
		if r == nil || !validHost(host) {
			warns = append(warns, fmt.Sprintf("%s: это не IP, не подсеть и не домен — строка пропущена", e))
			continue
		}
		addrs, err := r.LookupNetIP(ctx, "ip4", host)
		if err != nil || len(addrs) == 0 {
			warns = append(warns, fmt.Sprintf("%s: не удалось найти IP-адрес домена — строка пропущена", e))
			continue
		}
		for _, a := range addrs {
			out = append(out, netip.PrefixFrom(a.Unmap(), 32))
		}
	}
	return out, warns
}

func validHost(h string) bool {
	if len(h) == 0 || len(h) > 253 || !strings.Contains(h, ".") {
		return false
	}
	for _, c := range h {
		if !(c == '-' || c == '.' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

// span is an inclusive IPv4 address range; uint64 avoids overflow at 255.255.255.255.
type span struct{ lo, hi uint64 }

func spanOf(p netip.Prefix) span {
	p = p.Masked()
	b := p.Addr().As4()
	lo := uint64(b[0])<<24 | uint64(b[1])<<16 | uint64(b[2])<<8 | uint64(b[3])
	return span{lo, lo + (uint64(1) << (32 - p.Bits())) - 1}
}

// spans converts IPv4 prefixes into sorted, non-overlapping, non-adjacent ranges.
func spans(ps []netip.Prefix) []span {
	out := make([]span, 0, len(ps))
	for _, p := range ps {
		if p.Addr().Is4() {
			out = append(out, spanOf(p))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].lo < out[j].lo })
	merged := out[:0]
	for _, s := range out {
		if n := len(merged); n > 0 && s.lo <= merged[n-1].hi+1 {
			if s.hi > merged[n-1].hi {
				merged[n-1].hi = s.hi
			}
			continue
		}
		merged = append(merged, s)
	}
	return merged
}

// prefixesOf splits ranges into the minimal list of CIDR blocks.
func prefixesOf(ss []span) []netip.Prefix {
	var out []netip.Prefix
	for _, s := range ss {
		for lo := s.lo; lo <= s.hi; {
			bits := 32
			for bits > 0 {
				size := uint64(1) << (32 - (bits - 1))
				if lo%size != 0 || lo+size-1 > s.hi {
					break
				}
				bits--
			}
			out = append(out, netip.PrefixFrom(netip.AddrFrom4([4]byte{byte(lo >> 24), byte(lo >> 16), byte(lo >> 8), byte(lo)}), bits))
			lo += uint64(1) << (32 - bits)
		}
	}
	return out
}

// Subtract returns base minus every prefix in excl as a minimal sorted set of IPv4 prefixes.
// It works on merged ranges, so tens of thousands of exclusions stay fast.
func Subtract(base, excl []netip.Prefix) []netip.Prefix {
	b, e := spans(base), spans(excl)
	var out []span
	j := 0
	for _, r := range b {
		cur := r.lo
		for j < len(e) && e[j].hi < cur {
			j++
		}
		for k := j; k < len(e) && e[k].lo <= r.hi; k++ {
			if e[k].lo > cur {
				out = append(out, span{cur, e[k].lo - 1})
			}
			if e[k].hi+1 > cur {
				cur = e[k].hi + 1
			}
		}
		if cur <= r.hi {
			out = append(out, span{cur, r.hi})
		}
	}
	return prefixesOf(out)
}

// Merge returns the minimal sorted set of IPv4 prefixes covering exactly the same addresses.
func Merge(in []netip.Prefix) []netip.Prefix { return prefixesOf(spans(in)) }
