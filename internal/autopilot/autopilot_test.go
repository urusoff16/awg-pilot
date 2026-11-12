package autopilot

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"awgpilot/internal/store"
	"awgpilot/internal/subscription"
)

func TestWeak(t *testing.T) {
	cases := []struct {
		ms   []int64
		weak bool
	}{
		{[]int64{50, 60, 55}, false},                                // too few samples
		{[]int64{50, 60, 55, 70, 65, 80, 45, 50}, false},            // fine
		{[]int64{400, 500, 450, 420, 380, 600, 50, 410}, true},      // slow
		{[]int64{50, -1, -1, 60, -1, -1, 55, -1}, true},             // half lost
		{[]int64{50, -1, 60, 55, 70, -1, 65, 80, 45, 50}, false},    // a few lost
		{[]int64{-1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1}, true}, // nothing
	}
	for i, c := range cases {
		if w, _ := Weak(c.ms, 300); w != c.weak {
			t.Errorf("case %d: weak=%v, want %v", i, w, c.weak)
		}
	}
}

func profiles() []store.Profile {
	return []store.Profile{
		{ID: "sotka", Name: "Sotka", RawConfig: "x"},
		{ID: "velwet", Name: "Velwet", Sub: &store.Subscription{Servers: []subscription.Server{{ID: "s1", Name: "LTE-1", Group: "LTE"}}}},
		{ID: "empty", Name: "Пустой"},
	}
}

func TestWantFallback(t *testing.T) {
	s := store.Settings{AutoProfile: true, NetProfiles: map[string]string{"wifi": "sotka", "mobile": "velwet"}}
	ps := profiles()
	if w := Want(s, "wifi", ps); w != "sotka" {
		t.Fatalf("wifi: %q", w)
	}
	if w := Want(s, "wired", ps); w != "" {
		t.Fatalf("wired: %q", w)
	}
	if fb := Fallback(s, "sotka", ps); fb != "velwet" {
		t.Fatalf("fallback: %q", fb)
	}
	s.FallbackProfile = "sotka"
	if fb := Fallback(s, "sotka", ps); fb != "velwet" { // the fallback cannot be the profile itself
		t.Fatalf("fallback to itself: %q", fb)
	}
	if n := NextProfile(s, "wifi", "sotka", ps); n != "velwet" {
		t.Fatalf("next: %q", n)
	}
	s.AutoProfile = false
	if Want(s, "wifi", ps) != "" || NextProfile(s, "wifi", "sotka", ps) != "" {
		t.Fatal("auto off must choose nothing")
	}
}

// fakeHost records what the controller asks for.
type fakeHost struct {
	mu       sync.Mutex
	snap     Snapshot
	switches []string
	probeMs  int64
}

func (h *fakeHost) Snapshot() Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snap
}
func (h *fakeHost) SwitchProfile(id, why string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.switches = append(h.switches, id)
	h.snap.ActiveID = id
	return nil
}
func (h *fakeHost) ProbeAWG(string) (int64, error) { return h.probeMs, nil }
func (h *fakeHost) TestServers(string, string)     {}
func (h *fakeHost) ReconnectServer(string)         {}
func (h *fakeHost) Logf(f string, a ...any)        { fmt.Printf(f+"\n", a...) }
func (h *fakeHost) Notify(string, string)          {}

func TestControllerFlow(t *testing.T) {
	h := &fakeHost{probeMs: 40}
	h.snap = Snapshot{Settings: store.Settings{AutoProfile: true, NetProfiles: map[string]string{"wifi": "sotka", "mobile": "velwet"},
		WeakPingMs: 300, CheckEveryMin: 10}, Kind: "wifi", ActiveID: "sotka", Profiles: profiles(), Up: true, Answering: true, PingMs: 500}
	c := New(h)
	now := time.Now()
	c.tick(now) // learns the network; Sotka is already active
	if len(h.switches) != 0 {
		t.Fatalf("switched at start: %v", h.switches)
	}
	// Sotka stays slow past the settle time and the weak minute: the fallback takes over.
	for i := 1; i < 60 && len(h.switches) == 0; i++ {
		c.tick(now.Add(time.Duration(i) * Tick))
	}
	if len(h.switches) != 1 || h.switches[0] != "velwet" {
		t.Fatalf("weak Sotka: %v", h.switches)
	}
	// mobile network: Velwet is the profile for it already
	h.mu.Lock()
	h.snap.Kind = "mobile"
	h.mu.Unlock()
	if c.NetChanged("mobile") {
		t.Fatal("no switch expected on mobile")
	}
	// back on Wi-Fi: Sotka was weak a moment ago, so the fallback stays
	h.mu.Lock()
	h.snap.Kind = "wifi"
	h.mu.Unlock()
	c.NetChanged("wifi")
	if len(h.switches) != 1 {
		t.Fatalf("returned to a weak profile: %v", h.switches)
	}
	// ten minutes later the background check finds Sotka fast again and takes it back
	later := now.Add(13 * time.Minute)
	h.mu.Lock()
	h.snap.PingMs = 80
	h.mu.Unlock()
	// the first tick may start the server check of Velwet; the profile check follows on the next
	for i := 0; i < 3; i++ {
		c.tick(later.Add(time.Duration(i) * Tick))
		for j := 0; j < 100 && c.Checking(); j++ {
			time.Sleep(10 * time.Millisecond)
		}
	}
	h.mu.Lock()
	got := append([]string(nil), h.switches...)
	h.mu.Unlock()
	if len(got) != 2 || got[1] != "sotka" {
		t.Fatalf("no return to Sotka: %v", got)
	}
}

func TestManualHold(t *testing.T) {
	h := &fakeHost{probeMs: 40}
	h.snap = Snapshot{Settings: store.Settings{AutoProfile: true, NetProfiles: map[string]string{"wifi": "sotka"}, WeakPingMs: 300, CheckEveryMin: 10},
		Kind: "wifi", ActiveID: "sotka", Profiles: profiles(), Up: true, PingMs: 50}
	c := New(h)
	now := time.Now()
	c.tick(now)
	h.SwitchProfile("velwet", "by hand")
	c.Manual()
	c.tick(now.Add(20 * time.Minute))
	time.Sleep(50 * time.Millisecond)
	if len(h.switches) != 1 {
		t.Fatalf("manual choice overridden: %v", h.switches)
	}
}
