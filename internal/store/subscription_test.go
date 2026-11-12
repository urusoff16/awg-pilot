package store

import (
	"testing"
	"time"

	"awgpilot/internal/subscription"
)

func TestPick(t *testing.T) {
	servers := []subscription.Server{
		{ID: "a", Name: "SMART-A", Group: "SMART"},
		{ID: "b", Name: "#1 LTE", Group: "LTE"},
		{ID: "c", Name: "#2 LTE", Group: "LTE"},
		{ID: "d", Name: "#3 LTE", Group: "LTE"},
	}
	now := time.Now()
	s := &Subscription{Servers: servers, Measures: map[string]subscription.Measure{
		"a": {Ms: 100, At: now},
		"b": {Ms: -1, At: now}, // dead right now
		"c": {Ms: 600, At: now},
		"d": {Ms: 300, At: now},
	}}
	pick := func(kind string, failed map[string]bool) string {
		srv, ok := s.Pick(kind, failed)
		if !ok {
			return ""
		}
		return srv.ID
	}
	if got := pick("mobile", nil); got != "d" {
		t.Fatalf("mobile: %s, want the fastest LTE", got)
	}
	if got := pick("wifi", nil); got != "a" {
		t.Fatalf("wifi: %s, want the fastest outside LTE", got)
	}
	s.Current = "c" // a working current server is kept while the rule wants it
	if got := pick("mobile", nil); got != "c" {
		t.Fatalf("sticky: %s", got)
	}
	if got := pick("mobile", map[string]bool{"c": true}); got != "d" {
		t.Fatalf("after c failed: %s", got)
	}
	if got := pick("mobile", map[string]bool{"c": true, "d": true}); got != "a" {
		t.Fatalf("LTE used up: %s, want another group before a dead server", got)
	}
	if got := pick("mobile", map[string]bool{"a": true, "c": true, "d": true}); got != "b" {
		t.Fatalf("only the dead one left: %s", got)
	}
	if got := pick("mobile", map[string]bool{"a": true, "b": true, "c": true, "d": true}); got != "" {
		t.Fatalf("all failed: %s", got)
	}
	s.Rules = map[string]string{"wifi": "server:c"}
	if got := pick("wifi", nil); got != "c" {
		t.Fatalf("fixed: %s", got)
	}
	s.Rules = map[string]string{"wifi": "server:gone"} // a server that left the subscription
	if r := s.Rule("wifi"); r != "auto:!LTE" {
		t.Fatalf("stale fixed rule: %s", r)
	}
}

func TestMerge(t *testing.T) {
	s := &Subscription{Info: subscription.Info{Title: "Old"}, Servers: []subscription.Server{{ID: "a"}, {ID: "b"}},
		Measures: map[string]subscription.Measure{"a": {Ms: 1}, "b": {Ms: 2}}, Rules: map[string]string{"wifi": "auto"}}
	if n := s.Merge(subscription.Info{Title: "New"}, []subscription.Server{{ID: "b"}, {ID: "c"}}, "Old"); n != "New" {
		t.Fatalf("rename: %q", n)
	}
	if _, ok := s.Measures["a"]; ok || s.Measures["b"].Ms != 2 || s.Rules["wifi"] != "auto" {
		t.Fatalf("%+v", s)
	}
	if n := s.Merge(subscription.Info{Title: "Newer"}, nil, "My name"); n != "" {
		t.Fatalf("a name the user chose must stay: %q", n)
	}
}
