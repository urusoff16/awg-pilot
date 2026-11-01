// Package service is the privileged half of AWG Pilot: a Windows service that owns the tunnel
// adapter, routes, DNS, the kill switch, the "internet off when the VPN drops" lockdown and
// per-app firewall rules. The window talks to it over the ipc pipe and runs without admin rights.
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows/svc"

	"awgpilot/internal/flows"
	"awgpilot/internal/ipc"
	"awgpilot/internal/netstat"
	"awgpilot/internal/procnet"
	"awgpilot/internal/tunnel"
	"awgpilot/internal/wfp"
)

// Name is the Windows service name registered by the installer.
const Name = "AWGPilot"

// lockdownFlag survives service restarts and crashes: while it exists the service starts with
// the internet closed, because the user wanted no traffic outside the VPN.
func lockdownFlag() string {
	return filepath.Join(os.Getenv("ProgramData"), "AWGPilot", "lockdown")
}

// appsFile keeps the "only through VPN" list, so the rules come back with the service after a
// restart or crash even before the window sends the list again.
func appsFile() string {
	return filepath.Join(os.Getenv("ProgramData"), "AWGPilot", "vpn-only-apps.json")
}

// flowsDir keeps the traffic log: which programs sent what where, one file per day.
func flowsDir() string {
	return filepath.Join(os.Getenv("ProgramData"), "AWGPilot", "traffic")
}

type backend struct {
	tun   tunnel.Manager
	flows *flows.Recorder
	// flowsDone is closed when the traffic log has saved its last state after a stop.
	flowsDone chan struct{}
	apps      wfp.AppRules
	lock      wfp.Lockdown

	mu      sync.Mutex
	appList []string
	armed   bool // lockdown wanted for this connection
	lines   []string
	base    int // sequence number of lines[0]

	pingTarget atomic.Value // netip.Addr inside the tunnel
	pingMs     atomic.Int64
	pingFails  atomic.Int64
}

func newBackend() *backend {
	b := &backend{flows: flows.New(flowsDir(), procnet.New().Owner), flowsDone: make(chan struct{})}
	b.tun.Tap = b.flows
	b.pingMs.Store(-1)
	return b
}

// runFlows keeps the traffic log until ctx ends, then saves it.
func (b *backend) runFlows(ctx context.Context) {
	defer close(b.flowsDone)
	b.flows.Run(ctx.Done())
}

func (b *backend) Flows(day string) flows.Report { return b.flows.Report(day) }

func (b *backend) SetFlowLog(on bool) error {
	if err := b.flows.SetEnabled(on); err != nil {
		return fmt.Errorf("журнал трафика не переключён: %w", err)
	}
	b.logf("журнал трафика по программам %s", map[bool]string{true: "включён", false: "выключен"}[on])
	return nil
}

func (b *backend) ClearFlows() error { return b.flows.Clear() }

func (b *backend) logf(format string, args ...any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, time.Now().Format("15:04:05 ")+fmt.Sprintf(format, args...))
	if over := len(b.lines) - 500; over > 0 {
		b.lines = b.lines[over:]
		b.base += over
	}
}

func (b *backend) Logs(since int) ([]string, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	next := b.base + len(b.lines)
	if since < b.base {
		since = b.base
	}
	if since > next {
		since = next
	}
	return append([]string(nil), b.lines[since-b.base:]...), next
}

// restoreLockdown closes the internet at service start if the VPN was armed when it dropped.
func (b *backend) restoreLockdown() {
	if _, err := os.Stat(lockdownFlag()); err != nil {
		return
	}
	b.mu.Lock()
	b.armed = true
	b.mu.Unlock()
	if err := b.lock.Apply(0); err != nil {
		b.logf("%v", err)
		return
	}
	b.logf("интернет заблокирован: VPN был включён с защитой от обрыва — ждём переподключения")
}

// restoreApps reinstalls the saved per-app rules at service start (no tunnel yet: no network).
func (b *backend) restoreApps() {
	data, err := os.ReadFile(appsFile())
	if err != nil {
		return
	}
	var apps []string
	if json.Unmarshal(data, &apps) != nil || len(apps) == 0 {
		return
	}
	b.mu.Lock()
	b.appList = apps
	b.mu.Unlock()
	b.applyApps()
}

func (b *backend) setFlag(on bool) {
	p := lockdownFlag()
	if on {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("1"), 0o644)
	} else {
		os.Remove(p)
	}
}

func (b *backend) Up(p ipc.UpParams) error {
	err := b.tun.Up(tunnel.Options{
		RawConfig: p.RawConfig, AllowedIPs: p.AllowedIPs, FullTunnel: p.FullTunnel,
		KillSwitch: p.KillSwitch, BlockIPv6: p.BlockIPv6, DNS: p.DNS, MTU: p.MTU,
		Logf: b.logf,
	})
	if err != nil {
		b.logf("туннель не поднят: %v", err)
		return err // an existing lockdown (if any) stays in place
	}
	b.mu.Lock()
	b.armed = p.Lockdown
	b.mu.Unlock()
	switch {
	case p.Lockdown && p.FullTunnel:
		// while connected the lockdown acts as a kill switch that also outlives the tunnel
		if err := b.lock.Apply(b.tun.LUID()); err != nil {
			b.logf("%v", err)
		}
	default:
		// split tunneling must let excluded traffic out, so the lockdown only closes on a drop
		b.lock.Close()
	}
	b.setFlag(p.Lockdown)
	b.pingTarget.Store(pingTarget(p))
	b.pingMs.Store(-1)
	b.pingFails.Store(0)
	if err := b.applyApps(); err != nil {
		b.Down(false) // keeps an armed lockdown closed while the tunnel goes away
		return fmt.Errorf("туннель отключён, потому что не удалось применить правила для программ: %w", err)
	}
	return nil
}

// pingTarget is the tunnel's DNS server: always routed through the tunnel.
func pingTarget(p ipc.UpParams) netip.Addr {
	dns := p.DNS
	if len(dns) == 0 {
		if cfg, err := tunnel.ParseConfig(p.RawConfig); err == nil {
			for _, d := range cfg.Interface.DNS {
				if a, ok := netip.AddrFromSlice(d); ok {
					dns = append(dns, a.Unmap())
				}
			}
		}
	}
	for _, d := range dns {
		if d.Is4() {
			return d
		}
	}
	return netip.MustParseAddr("1.1.1.1")
}

// Down stops the tunnel. Unless release is set, an armed lockdown keeps the internet closed.
func (b *backend) Down(release bool) {
	wasUp := b.tun.IsUp()
	b.mu.Lock()
	armed := b.armed && !release
	if release {
		b.armed = false
	}
	b.mu.Unlock()
	if armed && wasUp {
		// Close the physical network while the tunnel still exists: in split mode nothing blocks
		// it before this point, and new connections must not slip out during the teardown.
		if err := b.lock.Apply(b.tun.LUID()); err != nil {
			b.logf("%v", err)
		}
	}
	b.tun.Down()
	if armed {
		if err := b.lock.Apply(0); err != nil {
			b.logf("%v", err)
		} else if wasUp {
			b.logf("туннель опущен, интернет заблокирован до восстановления VPN")
		}
	} else {
		b.lock.Close()
		b.setFlag(false)
		if wasUp {
			b.logf("туннель опущен")
		}
	}
	b.pingMs.Store(-1)
	b.applyApps()
}

// Unlock reopens the internet without connecting.
func (b *backend) Unlock() {
	b.mu.Lock()
	b.armed = false
	b.mu.Unlock()
	if b.lock.Active() && !b.tun.IsUp() {
		b.logf("блокировка интернета снята вручную")
	}
	b.lock.Close()
	b.setFlag(false)
}

func (b *backend) Stats() tunnel.Stats {
	st := b.tun.Stats()
	b.mu.Lock()
	st.Lockdown = b.armed
	b.mu.Unlock()
	st.Blocked = !st.Up && b.lock.Active()
	st.PingMs = b.pingMs.Load()
	st.PingFails = int(b.pingFails.Load())
	return st
}

func (b *backend) SetApps(apps []string) error {
	b.mu.Lock()
	b.appList = append([]string(nil), apps...)
	b.mu.Unlock()
	if data, err := json.Marshal(apps); err == nil {
		os.MkdirAll(filepath.Dir(appsFile()), 0o755)
		if err := os.WriteFile(appsFile(), data, 0o644); err != nil {
			b.logf("список программ «только через VPN» не сохранён: %v", err)
		}
	}
	return b.applyApps()
}

// applyApps installs per-app rules for the current tunnel state: through the tunnel only while
// it is up, no network at all while it is down.
func (b *backend) applyApps() error {
	b.mu.Lock()
	list := append([]string(nil), b.appList...)
	b.mu.Unlock()
	var apps []string
	for _, p := range list {
		// A program deleted since it was chosen must not lift the rules for all the others.
		if _, err := os.Stat(p); err != nil {
			b.logf("программа «только через VPN» не найдена, правило для неё пропущено: %s", p)
			continue
		}
		apps = append(apps, p)
	}
	luid := b.tun.LUID()
	if err := b.apps.Apply(apps, luid); err != nil {
		b.logf("%v", err)
		return err
	}
	if len(apps) > 0 {
		state := "выходят в сеть только через туннель"
		if luid == 0 {
			state = "отключены от сети до подключения VPN"
		}
		b.logf("программы «только через VPN» (%d) %s", len(apps), state)
	}
	return nil
}

// pinger measures the round trip through the tunnel every 5 seconds.
func (b *backend) pinger(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		target, ok := b.pingTarget.Load().(netip.Addr)
		if !ok || !target.IsValid() || !b.tun.IsUp() {
			continue
		}
		if rtt, err := netstat.Ping(target, 2*time.Second); err == nil {
			b.pingMs.Store(rtt.Milliseconds())
			b.pingFails.Store(0)
		} else {
			b.pingMs.Store(-1)
			b.pingFails.Add(1)
		}
	}
}

// shutdown runs when the service stops: the tunnel goes, but a requested lockdown is kept by
// the flag file and restored when the service starts again.
func (b *backend) shutdown() {
	b.tun.Down()
	b.lock.Close()
	b.apps.Close()
}

// waitFlows gives the traffic log a moment to write today's file after a stop.
func (b *backend) waitFlows() {
	select {
	case <-b.flowsDone:
	case <-time.After(3 * time.Second):
	}
}

// ---- Windows service plumbing -------------------------------------------------------------

type handler struct{ version string }

func (h *handler) Execute(_ []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	s <- svc.Status{State: svc.StartPending}
	b := newBackend()
	ctx, cancel := context.WithCancel(context.Background())
	b.restoreApps()
	b.restoreLockdown()
	go b.pinger(ctx)
	go b.runFlows(ctx)
	served := make(chan error, 1)
	go func() { served <- ipc.Serve(ctx, ipc.PipePath, b, h.version) }()
	b.logf("служба запущена (версия %s)", h.version)
	s <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				s <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				s <- svc.Status{State: svc.StopPending}
				cancel()
				b.shutdown()
				b.waitFlows()
				return false, 0
			}
		case err := <-served:
			// the control pipe is the only way to use the service; without it, stop with an error
			b.shutdown()
			cancel()
			b.waitFlows()
			if err != nil {
				return true, 1
			}
			return false, 0
		}
	}
}

// Run executes as the Windows service started by the Service Control Manager.
func Run(version string) error { return svc.Run(Name, &handler{version: version}) }

// RunConsole hosts the same backend in the foreground (needs an elevated console); used only
// for development. Ctrl+C stops it and removes the tunnel.
func RunConsole(version string) error {
	b := newBackend()
	b.restoreApps()
	b.restoreLockdown()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	go b.pinger(ctx)
	go b.runFlows(ctx)
	go func() {
		next := 0
		for ctx.Err() == nil {
			var lines []string
			lines, next = b.Logs(next)
			for _, l := range lines {
				fmt.Println(l)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()
	b.logf("консольный режим службы, Ctrl+C — остановить")
	err := ipc.Serve(ctx, ipc.PipePath, b, version)
	b.shutdown()
	cancel()
	b.waitFlows()
	return err
}
