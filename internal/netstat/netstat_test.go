package netstat

import (
	"net/netip"
	"testing"
	"time"
)

func TestMeterRatesAndHistory(t *testing.T) {
	m := NewMeter(3)
	t0 := time.Unix(1000, 0)
	m.Add(t0, 100, 50) // baseline only
	if last, h := m.Snapshot(); len(h) != 0 || last.Rx != 0 {
		t.Fatalf("baseline must not produce a point: %v", h)
	}
	m.Add(t0.Add(2*time.Second), 2100, 1050)
	last, _ := m.Snapshot()
	if last.Rx != 1000 || last.Tx != 500 {
		t.Fatalf("rate = %+v, want 1000/500 B/s", last)
	}
	for i := 3; i <= 6; i++ {
		m.Add(t0.Add(time.Duration(i)*time.Second), uint64(2100+i), 1050)
	}
	if _, h := m.Snapshot(); len(h) != 3 {
		t.Fatalf("history not trimmed: %d", len(h))
	}
	m.Add(t0.Add(10*time.Second), 5, 5) // counter reset: new baseline, no negative rate
	if last, _ := m.Snapshot(); last.Rx < 0 {
		t.Fatal("negative rate after reset")
	}
	m.Reset()
	if _, h := m.Snapshot(); len(h) != 0 {
		t.Fatal("reset kept history")
	}
}

func TestPingLoopback(t *testing.T) {
	rtt, err := Ping(netip.MustParseAddr("127.0.0.1"), time.Second)
	if err != nil {
		t.Fatalf("loopback ping failed: %v", err)
	}
	if rtt <= 0 || rtt > time.Second {
		t.Fatalf("odd rtt %v", rtt)
	}
	if _, err := Ping(netip.MustParseAddr("::1"), time.Second); err == nil {
		t.Fatal("IPv6 accepted")
	}
}
