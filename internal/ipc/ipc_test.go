package ipc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/ipc/namedpipe"
	"golang.org/x/sys/windows"

	"awgpilot/internal/flows"
	"awgpilot/internal/tunnel"
)

type fakeBackend struct {
	mu       sync.Mutex
	up       *UpParams
	apps     []string
	released bool
	unlocked bool
}

func (f *fakeBackend) Up(p UpParams) error {
	if p.RawConfig == "bad" {
		return errors.New("конфиг повреждён")
	}
	f.mu.Lock()
	f.up = &p
	f.mu.Unlock()
	return nil
}
func (f *fakeBackend) Down(release bool) {
	f.mu.Lock()
	f.up, f.released = nil, release
	f.mu.Unlock()
}
func (f *fakeBackend) Unlock() { f.mu.Lock(); f.unlocked = true; f.mu.Unlock() }
func (f *fakeBackend) Stats() tunnel.Stats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return tunnel.Stats{Up: f.up != nil, Endpoint: "192.0.2.1:51820", RxBytes: 42}
}
func (f *fakeBackend) SetApps(a []string) error { f.mu.Lock(); f.apps = a; f.mu.Unlock(); return nil }
func (f *fakeBackend) Logs(since int) ([]string, int) {
	all := []string{"a", "b", "c"}
	if since > len(all) {
		since = len(all)
	}
	return all[since:], len(all)
}
func (f *fakeBackend) Flows(day string) flows.Report { return flows.Report{Day: day} }
func (f *fakeBackend) SetFlowLog(bool) error         { return nil }
func (f *fakeBackend) ClearFlows() error             { return nil }

func TestRoundTrip(t *testing.T) {
	path := fmt.Sprintf(`\\.\pipe\AWGPilotTest-%d`, time.Now().UnixNano())
	b := &fakeBackend{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() { serveErr <- Serve(ctx, path, b, "test-1") }()
	c := &Client{Path: path}
	var v string
	var err error
	for i := 0; i < 50; i++ { // wait for the listener
		select {
		case e := <-serveErr:
			t.Fatalf("server stopped: %v", e)
		default:
		}
		if v, err = c.Version(); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || v != "test-1" {
		t.Fatalf("version: %q %v", v, err)
	}
	routes := []netip.Prefix{netip.MustParsePrefix("0.0.0.0/1"), netip.MustParsePrefix("128.0.0.0/1")}
	if err := c.Up(UpParams{RawConfig: "[Interface]", AllowedIPs: routes, KillSwitch: true, Lockdown: true}); err != nil {
		t.Fatal(err)
	}
	if st := c.Stats(); !st.Up || st.RxBytes != 42 {
		t.Fatalf("stats %+v", st)
	}
	if len(b.up.AllowedIPs) != 2 || b.up.AllowedIPs[1] != routes[1] || !b.up.KillSwitch || !b.up.Lockdown {
		t.Fatalf("params lost in transit: %+v", b.up)
	}
	if err := c.Up(UpParams{RawConfig: "bad"}); err == nil || err.Error() != "конфиг повреждён" {
		t.Fatalf("error not propagated: %v", err)
	}
	if err := c.SetApps([]string{`C:\x.exe`}); err != nil || len(b.apps) != 1 {
		t.Fatalf("apps: %v", err)
	}
	lines, next, err := c.Logs(1)
	if err != nil || len(lines) != 2 || next != 3 {
		t.Fatalf("logs %v %d %v", lines, next, err)
	}
	c.Down(true)
	if c.Stats().Up || !b.released {
		t.Fatal("down/release not delivered")
	}
	if err := c.Unlock(); err != nil || !b.unlocked {
		t.Fatalf("unlock: %v", err)
	}
}

func TestNoService(t *testing.T) {
	c := &Client{Path: `\\.\pipe\AWGPilotMissing-` + fmt.Sprint(time.Now().UnixNano())}
	if _, err := c.Version(); !errors.Is(err, ErrNoService) {
		t.Fatalf("err = %v", err)
	}
	if c.Stats().Up {
		t.Fatal("missing service must read as down")
	}
}

// A pipe created by an ordinary (non-elevated) program must not pass as the service.
func TestVerifyServerRejectsUserPipe(t *testing.T) {
	if isElevated() {
		t.Skip("an elevated test creates pipes owned by Administrators")
	}
	path := fmt.Sprintf(`\\.\pipe\AWGPilotTest-%d`, time.Now().UnixNano())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, path, &fakeBackend{}, "test")
	var conn net.Conn
	var err error
	for i := 0; i < 50; i++ {
		if conn, err = namedpipe.DialTimeout(path, time.Second); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := verifyServer(conn); err == nil {
		t.Fatal("a pipe owned by the current user was accepted")
	}
}

func isElevated() bool { return windows.GetCurrentProcessToken().IsElevated() }
