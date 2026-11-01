package flows

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"strings"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	protoTCP = 6
	protoUDP = 17
)

type packet struct {
	proto        uint8
	src, dst     netip.Addr
	sport, dport uint16
	payload      []byte // TCP/UDP payload
}

// parse reads the IP and TCP/UDP headers. IPv6 extension headers are not followed: such
// packets are counted under their next-header number without ports.
func parse(b []byte) (p packet, ok bool) {
	if len(b) < 1 {
		return p, false
	}
	var l4 []byte
	switch b[0] >> 4 {
	case 4:
		if len(b) < 20 {
			return p, false
		}
		ihl := int(b[0]&0x0f) * 4
		total := int(binary.BigEndian.Uint16(b[2:4]))
		if ihl < 20 || total < ihl || total > len(b) {
			return p, false
		}
		p.proto = b[9]
		p.src = netip.AddrFrom4([4]byte(b[12:16]))
		p.dst = netip.AddrFrom4([4]byte(b[16:20]))
		if binary.BigEndian.Uint16(b[6:8])&0x1fff != 0 { // a later fragment carries no ports
			return p, true
		}
		l4 = b[ihl:total]
	case 6:
		if len(b) < 40 {
			return p, false
		}
		total := 40 + int(binary.BigEndian.Uint16(b[4:6]))
		if total > len(b) {
			return p, false
		}
		p.proto = b[6]
		p.src = netip.AddrFrom16([16]byte(b[8:24]))
		p.dst = netip.AddrFrom16([16]byte(b[24:40]))
		l4 = b[40:total]
	default:
		return p, false
	}
	switch p.proto {
	case protoTCP:
		if len(l4) < 20 {
			return p, true
		}
		p.sport, p.dport = binary.BigEndian.Uint16(l4[0:2]), binary.BigEndian.Uint16(l4[2:4])
		if off := int(l4[12]>>4) * 4; off >= 20 && off <= len(l4) {
			p.payload = l4[off:]
		}
	case protoUDP:
		if len(l4) < 8 {
			return p, true
		}
		p.sport, p.dport = binary.BigEndian.Uint16(l4[0:2]), binary.BigEndian.Uint16(l4[2:4])
		p.payload = l4[8:]
	}
	return p, true
}

// dnsAnswers maps the addresses in a DNS response to the name that was asked for (not the
// CNAME targets, which are usually CDN names nobody recognises).
func dnsAnswers(msg []byte) map[netip.Addr]string {
	var pr dnsmessage.Parser
	h, err := pr.Start(msg)
	if err != nil || !h.Response {
		return nil
	}
	q, err := pr.Question()
	if err != nil {
		return nil
	}
	if pr.SkipAllQuestions() != nil {
		return nil
	}
	name := strings.TrimSuffix(strings.ToLower(q.Name.String()), ".")
	out := map[netip.Addr]string{}
	for {
		ah, err := pr.AnswerHeader()
		if err != nil {
			break
		}
		switch ah.Type {
		case dnsmessage.TypeA:
			if a, err := pr.AResource(); err == nil {
				out[netip.AddrFrom4(a.A)] = name
			}
		case dnsmessage.TypeAAAA:
			if a, err := pr.AAAAResource(); err == nil {
				out[netip.AddrFrom16(a.AAAA)] = name
			}
		default:
			if pr.SkipAnswer() != nil {
				return out
			}
		}
	}
	return out
}

// tlsServerName reads server_name from the start of a TLS ClientHello. done is false while more
// bytes are needed; done with an empty name means this is not a ClientHello or it has no name.
func tlsServerName(b []byte) (name string, done bool) {
	if len(b) < 6 {
		return "", false
	}
	if b[0] != 0x16 || b[1] != 3 || b[5] != 1 { // handshake record, ClientHello
		return "", true
	}
	r := reader{b: b[9:]} // skip the record header (5) and the handshake header (4)
	r.skip(2 + 32)        // version, random
	r.skip(int(r.u8()))   // session id
	r.skip(int(r.u16()))  // cipher suites
	r.skip(int(r.u8()))   // compression methods
	left := int(r.u16())  // extensions length
	for left >= 4 && !r.short {
		typ, size := r.u16(), int(r.u16())
		left -= 4 + size
		if typ != 0 {
			r.skip(size)
			continue
		}
		ext := reader{b: r.take(size)}
		if r.short {
			break
		}
		ext.u16() // list length
		for !ext.short {
			kind, n := ext.u8(), int(ext.u16())
			host := ext.take(n)
			if !ext.short && kind == 0 {
				return string(host), true
			}
		}
		return "", true
	}
	if r.short {
		return "", false
	}
	return "", true
}

// httpHost reads the Host header of a plain HTTP request.
func httpHost(b []byte) (string, bool) {
	sp := bytes.IndexByte(b, ' ')
	if sp < 3 || sp > 7 || !isUpper(b[:sp]) {
		return "", true
	}
	end := bytes.Index(b, []byte("\r\n\r\n"))
	head := b
	if end >= 0 {
		head = b[:end+2]
	}
	for _, line := range bytes.Split(head, []byte("\r\n"))[1:] {
		k, v, ok := bytes.Cut(line, []byte(":"))
		if ok && strings.EqualFold(string(k), "host") {
			h := strings.TrimSpace(string(v))
			if i := strings.LastIndexByte(h, ':'); i > 0 && !strings.Contains(h[i:], "]") {
				h = h[:i]
			}
			return h, true
		}
	}
	return "", end >= 0
}

func isUpper(b []byte) bool {
	for _, c := range b {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}

// reader walks a byte slice and sets short instead of panicking when it runs out.
type reader struct {
	b     []byte
	short bool
}

func (r *reader) take(n int) []byte {
	if r.short || n > len(r.b) {
		r.short = true
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

func (r *reader) skip(n int) { r.take(n) }

func (r *reader) u8() uint8 {
	if v := r.take(1); v != nil {
		return v[0]
	}
	return 0
}

func (r *reader) u16() uint16 {
	if v := r.take(2); v != nil {
		return binary.BigEndian.Uint16(v)
	}
	return 0
}
