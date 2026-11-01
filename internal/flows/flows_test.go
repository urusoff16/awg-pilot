package flows

import (
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

var (
	local  = netip.MustParseAddr("10.8.1.2")
	remote = netip.MustParseAddr("142.250.74.110")
)

// ipv4 builds an IPv4 packet with a TCP (flags-only header) or UDP header around payload.
func ipv4(proto uint8, src, dst netip.Addr, sport, dport uint16, payload []byte) []byte {
	l4len := 8
	if proto == protoTCP {
		l4len = 20
	}
	b := make([]byte, 20+l4len+len(payload))
	b[0] = 0x45
	binary.BigEndian.PutUint16(b[2:], uint16(len(b)))
	b[8], b[9] = 64, proto
	s, d := src.As4(), dst.As4()
	copy(b[12:], s[:])
	copy(b[16:], d[:])
	l4 := b[20:]
	binary.BigEndian.PutUint16(l4[0:], sport)
	binary.BigEndian.PutUint16(l4[2:], dport)
	if proto == protoTCP {
		l4[12] = 5 << 4
	} else {
		binary.BigEndian.PutUint16(l4[4:], uint16(8+len(payload)))
	}
	copy(l4[l4len:], payload)
	return b
}

func clientHello(t *testing.T, server string) []byte {
	t.Helper()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go tls.Client(a, &tls.Config{ServerName: server, InsecureSkipVerify: true}).Handshake()
	buf := make([]byte, 16384)
	b.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := b.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	// net.Pipe delivers one Write per Read; the record may still arrive in parts
	for n < 5 || n < 5+int(binary.BigEndian.Uint16(buf[3:5])) {
		m, err := b.Read(buf[n:])
		if err != nil {
			t.Fatal(err)
		}
		n += m
	}
	return buf[:n]
}

func TestTLSServerName(t *testing.T) {
	hello := clientHello(t, "gemini.google.com")
	if name, done := tlsServerName(hello); !done || name != "gemini.google.com" {
		t.Fatalf("got %q done=%v", name, done)
	}
	if _, done := tlsServerName(hello[:60]); done {
		t.Fatal("a cut ClientHello must ask for more bytes")
	}
	if name, done := tlsServerName([]byte("GET / HTTP/1.1\r\n")); !done || name != "" {
		t.Fatal("plain HTTP taken for TLS")
	}
}

func TestHTTPHost(t *testing.T) {
	if h, done := httpHost([]byte("GET /x HTTP/1.1\r\nUser-Agent: a\r\nHost: example.org:8080\r\n\r\n")); !done || h != "example.org" {
		t.Fatalf("got %q", h)
	}
	if _, done := httpHost([]byte("\x16\x03\x01")); !done {
		t.Fatal("binary data must stop the search")
	}
}

func dnsResponse(t *testing.T, name string, addrs ...[4]byte) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{Response: true})
	b.StartQuestions()
	n := dnsmessage.MustNewName(name + ".")
	b.Question(dnsmessage.Question{Name: n, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET})
	b.StartAnswers()
	b.CNAMEResource(dnsmessage.ResourceHeader{Name: n, Class: dnsmessage.ClassINET}, dnsmessage.CNAMEResource{CNAME: dnsmessage.MustNewName("cdn.example.net.")})
	for _, a := range addrs {
		b.AResource(dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName("cdn.example.net."), Class: dnsmessage.ClassINET}, dnsmessage.AResource{A: a})
	}
	msg, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func TestRecorder(t *testing.T) {
	dir := t.TempDir()
	owners := map[uint16]string{50000: "chrome.exe", 50001: "svchost.exe (DoSvc)"}
	r := New(dir, func(_ uint8, _ netip.Addr, lport uint16, _ netip.Addr, _ uint16) string { return owners[lport] })
	r.Packet(ipv4(protoTCP, local, remote, 50000, 443, nil), true)
	if len(r.flows) != 0 {
		t.Fatal("counted while disabled")
	}
	if err := r.SetEnabled(true); err != nil {
		t.Fatal(err)
	}

	dnsSrv := netip.MustParseAddr("1.1.1.1")
	upload := netip.MustParseAddr("203.0.113.9")
	r.Packet(ipv4(protoUDP, dnsSrv, local, 53, 60000, dnsResponse(t, "dl.delivery.mp.microsoft.com", upload.As4())), false)
	r.Packet(ipv4(protoTCP, local, remote, 50000, 443, clientHello(t, "gemini.google.com")), true)
	r.Packet(ipv4(protoTCP, remote, local, 443, 50000, make([]byte, 1000)), false)
	for i := 0; i < 3; i++ {
		r.Packet(ipv4(protoTCP, local, upload, 50001, 443, make([]byte, 1300)), true)
	}
	r.Packet(ipv4(protoTCP, local, netip.MustParseAddr("198.51.100.1"), 50002, 80, nil), true)
	r.resolve()

	rep := r.Report("")
	if !rep.Enabled || len(rep.Days) == 0 || rep.Days[0] != time.Now().Format(dayLayout) {
		t.Fatalf("report header %+v", rep)
	}
	got := map[string]Row{}
	for _, row := range rep.Rows {
		got[row.App+" "+row.Host] = row
	}
	if row := got["svchost.exe (DoSvc) dl.delivery.mp.microsoft.com"]; row.Tx != 3*(40+1300) || row.Conns != 1 || row.Port != 443 || row.Proto != "TCP" {
		t.Fatalf("upload row %+v (rows %+v)", row, rep.Rows)
	}
	if row := got["chrome.exe gemini.google.com"]; row.Rx != 1040 || row.Tx == 0 {
		t.Fatalf("chrome row %+v", row)
	}
	if rep.Rows[0].App != "svchost.exe (DoSvc)" {
		t.Fatalf("rows not sorted by traffic: %+v", rep.Rows)
	}
	// the port-80 connection has no owner yet: it waits, then becomes "unknown"
	if _, ok := got[Unknown+" 198.51.100.1"]; ok {
		t.Fatal("unresolved connection reported before its retries")
	}
	r.resolve()
	r.resolve()
	if rep = r.Report(""); rep.Rows[len(rep.Rows)-1].App != Unknown {
		t.Fatalf("unknown row missing: %+v", rep.Rows)
	}

	// more traffic on a known connection adds bytes but not connections
	r.Packet(ipv4(protoTCP, local, upload, 50001, 443, make([]byte, 1300)), true)
	r.fold(time.Now())
	r.save()
	r2 := New(dir, nil)
	if !r2.Enabled() {
		t.Fatal("enabled flag not kept")
	}
	rep = r2.Report("")
	if rep.Rows[0].Tx != 4*(40+1300) || rep.Rows[0].Conns != 1 {
		t.Fatalf("saved row %+v", rep.Rows[0])
	}
	if err := r2.Clear(); err != nil {
		t.Fatal(err)
	}
	if rep = r2.Report(""); len(rep.Rows) != 0 {
		t.Fatalf("clear kept rows: %+v", rep.Rows)
	}
}

func TestParseRejectsJunk(t *testing.T) {
	for _, b := range [][]byte{nil, {0x45}, make([]byte, 19), {0x60, 0, 0, 0}} {
		if _, ok := parse(b); ok {
			t.Fatalf("accepted %x", b)
		}
	}
	p, ok := parse(ipv4(protoUDP, local, remote, 1, 2, []byte("x")))
	if !ok || p.sport != 1 || p.dport != 2 || string(p.payload) != "x" {
		t.Fatalf("udp parse %+v", p)
	}
}

func TestPeriods(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	day := func(back int) string { return now.AddDate(0, 0, -back).Format(dayLayout) }
	write := func(d string, rows []Row) {
		b, _ := json.Marshal(rows)
		if err := os.WriteFile(filepath.Join(dir, d+".json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(day(1), []Row{{App: "a.exe", Host: "x.com", Port: 443, Proto: "TCP", Tx: 100, Rx: 10, Conns: 1, Last: 5}})
	write(day(20), []Row{{App: "a.exe", Host: "x.com", Port: 443, Proto: "TCP", Tx: 1000, Rx: 1, Conns: 2, Last: 9}})
	write(day(200), []Row{{App: "b.exe", Host: "y.com", Port: 80, Proto: "TCP", Tx: 7}})
	r := New(dir, nil)
	sum := func(period string) (tx uint64, conns int) {
		for _, row := range r.Report(period).Rows {
			tx += row.Tx
			conns += row.Conns
		}
		return
	}
	for period, want := range map[string]uint64{"": 0, "week": 100, "month": 1100, "6m": 1100, "12m": 1107, "all": 1107, day(20): 1000} {
		if got, _ := sum(period); got != want {
			t.Errorf("%q: tx %d, want %d", period, got, want)
		}
	}
	if rows := r.Report("month").Rows; len(rows) != 1 || rows[0].Conns != 3 || rows[0].Last != 9 {
		t.Fatalf("month rows not merged: %+v", rows)
	}
	if rep := r.Report("bogus"); rep.Day != "" || len(rep.Rows) != 0 {
		t.Fatalf("bad period not treated as today: %+v", rep)
	}
}
