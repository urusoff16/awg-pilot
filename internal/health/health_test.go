package health

import (
	"testing"
	"time"
)

func TestDead(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	cases := []struct {
		name string
		in   Input
		want bool
	}{
		{"fresh tunnel is not judged", Input{Now: at(5), Since: t0}, false},
		{"no handshake at all", Input{Now: at(21), Since: t0}, true},
		{"handshake pending, still early", Input{Now: at(15), Since: t0}, false},
		{"pings lost", Input{Now: at(60), Since: t0, LastHandshake: at(1), PingWorks: true, PingFails: 3}, true},
		{"two pings lost is fine", Input{Now: at(60), Since: t0, LastHandshake: at(1), PingWorks: true, PingFails: 2}, false},
		{"two pings lost after a network switch", Input{Now: at(60), Since: t0, LastHandshake: at(1), PingWorks: true, PingFails: 2, NetChangedAt: at(45)}, true},
		{"network switch long ago", Input{Now: at(200), Since: t0, LastHandshake: at(150), PingWorks: true, PingFails: 2, NetChangedAt: at(45)}, false},
		{"server without ICMP, recent handshake", Input{Now: at(100), Since: t0, LastHandshake: at(1), PingFails: 9}, false},
		{"server without ICMP, stale handshake", Input{Now: at(300), Since: t0, LastHandshake: at(1), PingFails: 9}, true},
		{"just after a recovery step", Input{Now: at(65), Since: t0, LastHandshake: at(1), PingWorks: true, PingFails: 5, ActionAt: at(60)}, false},
	}
	for _, c := range cases {
		if got, _ := Dead(c.in); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

func TestPlan(t *testing.T) {
	cases := []struct {
		stage        int
		key, profile bool
		want         Step
	}{
		{0, true, true, Reconnect},
		{1, false, false, Reconnect},
		{2, true, true, RefreshConfig},
		{2, false, true, NextProfile},
		{2, false, false, Pause},
		{3, true, true, NextProfile},
		{3, true, false, OtherServer},
		{4, true, false, OtherServer},
		{5, true, true, Pause},
	}
	for _, c := range cases {
		if got := Plan(c.stage, c.key, c.profile); got != c.want {
			t.Errorf("Plan(%d, %v, %v) = %v, want %v", c.stage, c.key, c.profile, got, c.want)
		}
	}
}

func TestRecovered(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	in := Input{Now: at(30), Since: at(10), ActionAt: at(9), LastHandshake: at(11), PingWorks: true}
	if !Recovered(in) {
		t.Fatal("new tunnel with handshake and pings not recovered")
	}
	in.PingFails = 1
	if Recovered(in) {
		t.Fatal("lost ping counted as recovered")
	}
	in = Input{Now: at(15), Since: at(10), ActionAt: at(9), LastHandshake: at(11)}
	if Recovered(in) {
		t.Fatal("judged during grace")
	}
	in = Input{Now: at(100), Since: t0, ActionAt: at(60), LastHandshake: at(5), PingFails: 9}
	if Recovered(in) {
		t.Fatal("old handshake counted as recovered")
	}
}
