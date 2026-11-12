package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
	"golang.org/x/sys/windows"

	"awgpilot/internal/geoip"
	"awgpilot/internal/health"
	"awgpilot/internal/netx"
	"awgpilot/internal/routing"
	"awgpilot/internal/store"
	"awgpilot/internal/subscription"
	"awgpilot/internal/subui"
	"awgpilot/internal/tunnel"
	"awgpilot/internal/xray"
)

// subState is what the window keeps about proxy subscriptions besides the saved profile.
type subState struct {
	mu      sync.Mutex
	failed  map[string]bool // servers that did not work in this recovery round
	kind    string          // network kind the tunnel was brought up for
	testing atomic.Bool     // a server check is running
	// lastCheck is when servers were last checked for a recovery or a connect.
	lastCheck time.Time
	tested    atomic.Int32 // servers checked so far in it
	total     atomic.Int32
}

// subTestChunk servers go to the service per request, so the list fills in as it goes.
const subTestChunk = 8

// ---- adding and updating -------------------------------------------------------------------

func winVersion() string {
	v := windows.RtlGetVersion()
	return fmt.Sprintf("%d.%d.%d", v.MajorVersion, v.MinorVersion, v.BuildNumber)
}

// subHeaders identify AWG Pilot to the provider; the HWID is AWG Pilot's own device ID.
func (a *App) subHeaders() subscription.Headers {
	a.mu.Lock()
	defer a.mu.Unlock()
	return subscription.Headers{UserAgent: a.userAgent(), HWID: a.settings.DeviceID, OS: "Windows", OSVersion: winVersion(), Model: "AWG Pilot"}
}

// addSubscription adds a subscription URL or server links (one per line) as a profile and starts
// checking its servers. A URL that is already saved is updated instead.
func (a *App) addSubscription(text string) error {
	text = strings.TrimSpace(text)
	var info subscription.Info
	var servers []subscription.Server
	link := ""
	low := strings.ToLower(text)
	if strings.HasPrefix(low, "https://") || strings.HasPrefix(low, "http://") {
		link = text
		ctx, cancel := a.reqCtx()
		var err error
		info, servers, err = subscription.Fetch(ctx, netx.Client, link, a.subHeaders())
		cancel()
		if err != nil {
			a.logf("подписка не загрузилась: %v", err)
			return err
		}
	} else {
		var skipped []string
		servers, skipped = subscription.Parse([]byte(text))
		if len(servers) == 0 {
			if len(skipped) > 0 {
				return errors.New(skipped[0])
			}
			return errors.New("ссылка на сервер не распознана")
		}
	}
	name := subui.Name(info, link, servers)
	var id string
	err := a.switchProfile(func() error {
		a.mu.Lock()
		defer a.mu.Unlock()
		if link != "" {
			for i := range a.secrets.Profiles {
				p := &a.secrets.Profiles[i]
				if p.Sub != nil && p.Sub.URL == link {
					if n := p.Sub.Merge(info, servers, p.Name); n != "" {
						p.Name = n
					}
					a.secrets.ActiveID, id = p.ID, p.ID
					a.activateLocked()
					a.logf("подписка «%s» уже сохранена — обновлена и выбрана (%d серверов)", p.Name, len(servers))
					return a.saveSecrets()
				}
			}
		}
		a.addProfileLocked(store.Profile{Name: name, Sub: &store.Subscription{URL: link, Info: info, Servers: servers, Updated: time.Now()}})
		id = a.secrets.ActiveID
		a.logf("добавлена подписка «%s»: %d серверов (%s)", name, len(servers), strings.Join(subscription.Groups(servers), ", "))
		return a.saveSecrets()
	})
	if err == nil {
		go a.testServers(id, nil)
	}
	return err
}

// UpdateSubscription downloads the active profile's server list again.
func (a *App) UpdateSubscription() error {
	a.mu.Lock()
	id := a.secrets.ActiveID
	a.mu.Unlock()
	return a.updateSubscription(id)
}

func (a *App) updateSubscription(id string) error {
	a.mu.Lock()
	p := a.profileByID(id)
	if p == nil || p.Sub == nil {
		a.mu.Unlock()
		return errors.New("у профиля нет подписки")
	}
	link := p.Sub.URL
	a.mu.Unlock()
	if link == "" {
		return errors.New("серверы добавлены ссылками, а не подпиской — обновлять нечего")
	}
	ctx, cancel := a.reqCtx()
	info, servers, err := subscription.Fetch(ctx, netx.Client, link, a.subHeaders())
	cancel()
	a.mu.Lock()
	defer a.mu.Unlock()
	p = a.profileByID(id)
	if p == nil || p.Sub == nil {
		return errors.New("профиль удалён, пока загружалась подписка")
	}
	if err != nil {
		a.logf("подписка «%s» не обновилась: %v", p.Name, err)
		return err
	}
	before := len(p.Sub.Servers)
	if n := p.Sub.Merge(info, servers, p.Name); n != "" {
		p.Name = n
	}
	a.logf("подписка «%s» обновлена: серверов %d (было %d)", p.Name, len(servers), before)
	a.trayProfilesLocked()
	return a.saveSecrets()
}

// ---- checking servers ----------------------------------------------------------------------

// TestServers checks every server of the active profile in the background; the list shows the
// results as they come.
func (a *App) TestServers() error {
	a.mu.Lock()
	id := a.secrets.ActiveID
	p := a.secrets.Active()
	ok := p != nil && p.Sub != nil
	a.mu.Unlock()
	if !ok {
		return errors.New("у активного профиля нет серверов подписки")
	}
	if a.sub.testing.Load() {
		return errors.New("проверка серверов уже идёт")
	}
	go a.testServers(id, nil)
	return nil
}

// testServers measures the servers of profile id (only those in only, if given) through the
// service and stores the results. It returns when all are done.
func (a *App) testServers(id string, only []subscription.Server) {
	if !a.sub.testing.CompareAndSwap(false, true) {
		return
	}
	defer a.sub.testing.Store(false)
	a.mu.Lock()
	p := a.profileByID(id)
	if p == nil || p.Sub == nil {
		a.mu.Unlock()
		return
	}
	list := only
	if list == nil {
		list = append([]subscription.Server(nil), p.Sub.Servers...)
	}
	name := p.Name
	a.mu.Unlock()
	a.sub.tested.Store(0)
	a.sub.total.Store(int32(len(list)))
	a.sub.mu.Lock()
	a.sub.lastCheck = time.Now()
	a.sub.mu.Unlock()
	ok := 0
	for start := 0; start < len(list); start += subTestChunk {
		chunk := list[start:min(start+subTestChunk, len(list))]
		links := make([]string, len(chunk))
		for i, s := range chunk {
			links[i] = s.Link
		}
		res, err := a.tun.c.TestServers(links)
		if err != nil {
			a.logf("проверка серверов не удалась: %v", err)
			return
		}
		now := time.Now()
		a.mu.Lock()
		if p := a.profileByID(id); p != nil && p.Sub != nil {
			for i, r := range res {
				p.Sub.SetMeasure(chunk[i].ID, subscription.Measure{Ms: r.Ms, Err: r.Error, At: now})
				if r.Ms > 0 {
					ok++
				}
			}
		}
		a.mu.Unlock()
		a.sub.tested.Add(int32(len(chunk)))
	}
	a.mu.Lock()
	a.saveSecrets()
	a.mu.Unlock()
	a.logf("серверы «%s» проверены: отвечают %d из %d", name, ok, len(list))
}

// ---- choosing and using a server -----------------------------------------------------------

// SetServerRule sets how servers are chosen on a kind of network ("wifi", "mobile", "wired"):
// "auto", "auto:GROUP", "auto:!GROUP" or "server:ID". "" returns to the default. When the VPN
// is on and the rule is for the current network, it reconnects to follow the new rule.
func (a *App) SetServerRule(kind, rule string) error {
	valid := false
	for _, k := range subscription.NetKinds {
		valid = valid || k == kind
	}
	if !valid {
		return errors.New("неизвестный вид сети")
	}
	a.mu.Lock()
	p := a.secrets.Active()
	if p == nil || p.Sub == nil {
		a.mu.Unlock()
		return errors.New("у активного профиля нет серверов подписки")
	}
	if id := subscription.FixedID(rule); id != "" && subscription.Find(p.Sub.Servers, id) == nil {
		a.mu.Unlock()
		return errors.New("сервер не найден в подписке")
	}
	if p.Sub.Rules == nil {
		p.Sub.Rules = map[string]string{}
	}
	if rule == "" {
		delete(p.Sub.Rules, kind)
	} else {
		p.Sub.Rules[kind] = rule
	}
	err := a.saveSecrets()
	a.mu.Unlock()
	if err != nil {
		return err
	}
	if a.tun.IsUp() && a.currentKind() == kind {
		a.sub.mu.Lock()
		a.sub.failed = nil
		a.sub.mu.Unlock()
		a.reconnectFresh()
	}
	return nil
}

// UseServer connects through one server now and keeps it for the current kind of network.
func (a *App) UseServer(id string) error {
	if err := a.SetServerRule(a.currentKind(), "server:"+id); err != nil {
		return err
	}
	if !a.tun.IsUp() {
		return a.Connect()
	}
	return nil
}

// currentKind is the network kind rules are looked up for ("wired" when it cannot be told).
func (a *App) currentKind() string {
	if k := tunnel.NetKind(); k != "" {
		return k
	}
	return subscription.NetWired
}

// reconnectFresh restarts the tunnel and lets it choose the server again (not keeping the current).
func (a *App) reconnectFresh() {
	a.tunOp.Lock()
	defer a.tunOp.Unlock()
	a.disconnectLocked(false)
	a.mu.Lock()
	if p := a.secrets.Active(); p != nil && p.Sub != nil {
		p.Sub.Current = ""
	}
	a.mu.Unlock()
	if err := a.connectLocked(); err != nil {
		a.logf("переподключение не удалось: %v", err)
	}
}

// connectSubLocked brings the tunnel up through a server of the active subscription. Callers
// hold tunOp.
func (a *App) connectSubLocked(id string, settings store.Settings) error {
	kind := a.currentKind()
	a.mu.Lock()
	p := a.profileByID(id)
	if p == nil || p.Sub == nil || len(p.Sub.Servers) == 0 {
		a.mu.Unlock()
		return errors.New("в подписке нет серверов: нажмите «Обновить подписку»")
	}
	rule := p.Sub.Rule(kind)
	need := subscription.NeedTest(p.Sub.Servers, rule, p.Sub.Measures, time.Now())
	preferred := p.Sub.Preferred(kind)
	profName := p.Name
	a.mu.Unlock()
	// Without fresh checks the "fastest" server is a guess: check the ones the rule wants first.
	// Only with the tunnel down, so that the checks go straight over the network.
	if need && !a.tun.IsUp() && len(preferred) > 0 && !a.sub.testing.Load() {
		a.logf("проверяю серверы для сети «%s»…", subui.NetName(kind))
		a.testServers(id, preferred)
	}

	dns := dnsServers(settings)
	if len(dns) == 0 { // a subscription brings no DNS servers of its own
		for _, d := range dnsPresets["cloudflare"] {
			dns = append(dns, netip.MustParseAddr(d))
		}
	}
	plan, err := a.routePlan(settings, dns)
	if err != nil {
		return err
	}
	// A server whose name does not resolve is skipped for the next one.
	for attempt := 0; attempt < 5; attempt++ {
		a.mu.Lock()
		p = a.profileByID(id)
		if p == nil || p.Sub == nil {
			a.mu.Unlock()
			return errors.New("профиль удалён")
		}
		srv, ok := p.Sub.Pick(kind, a.failedSet())
		servers := p.Sub.Servers
		a.mu.Unlock()
		if !ok {
			return errors.New("ни один сервер подписки не ответил — проверьте интернет или обновите подписку")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		addr, err := xray.Resolve(ctx, srv.Host)
		cancel()
		if err != nil {
			a.logf("адрес сервера «%s» (%s) не найден: %v", srv.Name, srv.Host, err)
			a.markFailed(srv.ID)
			continue
		}
		a.logf("подключаюсь: профиль «%s», сеть «%s», выбор «%s», сервер «%s» (%s:%d)", profName, subui.NetName(kind), subui.RuleName(rule, servers), srv.Name, srv.Host, srv.Port)
		err = a.tun.Up(tunnel.Options{
			Proxy: srv.Link, ProxyAddr: addr,
			AllowedIPs: plan.AllowedIPs, FullTunnel: plan.FullTunnel,
			KillSwitch: settings.KillSwitch, BlockIPv6: settings.BlockIPv6,
			DNS: dns, MTU: settings.MTU, Lockdown: settings.LockdownOnDrop,
			Logf: a.logf,
		})
		if err != nil {
			a.logf("подключение через «%s» не удалось: %v", srv.Name, err)
			return err
		}
		a.mu.Lock()
		if p := a.profileByID(id); p != nil && p.Sub != nil {
			p.Sub.Current = srv.ID
			a.saveSecrets()
		}
		a.mu.Unlock()
		a.sub.mu.Lock()
		a.sub.kind = kind
		a.sub.mu.Unlock()
		a.meter.Reset()
		a.pingMs.Store(-1)
		return nil
	}
	return errors.New("адреса серверов не находятся — проверьте интернет")
}

// failedSet is a copy of the servers that failed in this round.
func (a *App) failedSet() map[string]bool {
	a.sub.mu.Lock()
	defer a.sub.mu.Unlock()
	out := make(map[string]bool, len(a.sub.failed))
	for k, v := range a.sub.failed {
		out[k] = v
	}
	return out
}

func (a *App) markFailed(id string) {
	a.sub.mu.Lock()
	if a.sub.failed == nil {
		a.sub.failed = map[string]bool{}
	}
	a.sub.failed[id] = true
	a.sub.mu.Unlock()
}

// routePlan works out what goes through the tunnel (split tunnel, Russia, LAN).
func (a *App) routePlan(settings store.Settings, tunnelDNS []netip.Addr) (routing.Result, error) {
	var bypass []netip.Prefix
	if settings.Split.BypassRussia && settings.Split.Mode != routing.ModeInclude {
		ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
		list, err := geoip.Get(ctx, filepath.Join(a.st.Dir(), "ru-ipv4.txt"))
		cancel()
		switch {
		case list == nil:
			return routing.Result{}, fmt.Errorf("не удалось получить список российских сетей: %w", err)
		case err != nil:
			a.logf("список российских сетей: %v", err)
		}
		bypass = list.Prefixes
		a.mu.Lock()
		a.russiaStatus = fmt.Sprintf("%d сетей, обновлён %s", len(list.Prefixes), list.FetchedAt.Local().Format("02.01.2006"))
		a.mu.Unlock()
	}
	ctx, cancel := a.reqCtx()
	plan, err := routing.Compute(ctx, settings.Split, tunnelDNS, netx.Resolver, bypass)
	cancel()
	if err != nil {
		return routing.Result{}, err
	}
	if len(bypass) > 0 {
		a.logf("Россия мимо VPN: %d российских сетей напрямую, маршрутов через VPN: %d", len(bypass), len(plan.AllowedIPs))
	}
	a.mu.Lock()
	a.warnings = plan.Warnings
	a.mu.Unlock()
	for _, w := range plan.Warnings {
		a.logf("раздельный туннель: %s", w)
	}
	if settings.KillSwitch && !plan.FullTunnel {
		a.logf("kill switch не действует, пока включён раздельный туннель или «локальная сеть мимо VPN»")
	}
	return plan, nil
}

// ---- recovery ------------------------------------------------------------------------------

// subRecover is the watchdog's step for a subscription profile whose server stopped answering:
// reconnect once, then go through the other servers (fastest first), then download the list
// again and check everything, then other profiles. It returns the new stage.
func (a *App) subRecover(stage int, why string, profiles int, neverAnswered bool) {
	a.mu.Lock()
	p := a.secrets.Active()
	if p == nil || p.Sub == nil {
		a.mu.Unlock()
		return
	}
	cur := subscription.Find(p.Sub.Servers, p.Sub.Current)
	curName := "—"
	if cur != nil {
		curName = cur.Name
	}
	id, hasURL := p.ID, p.Sub.URL != ""
	a.mu.Unlock()

	switch {
	case stage == 0 && !neverAnswered:
		a.logf("%s — переподключаюсь к «%s»", why, curName)
		a.notify("Связь с сервером потеряна", "Переподключаюсь…", "")
		a.setStage(1)
		a.reconnect()
		return
	case stage <= 1:
		if cur != nil {
			a.markFailed(cur.ID)
			a.mu.Lock()
			if p := a.profileByID(id); p != nil && p.Sub != nil {
				p.Sub.SetMeasure(cur.ID, subscription.Measure{Ms: -1, Err: "перестал отвечать", At: time.Now()})
			}
			a.mu.Unlock()
		}
		// Check the servers the rule wants at once (in parallel, a few seconds) rather than try
		// them one by one: on a jammed mobile network most of them may be down.
		a.sub.mu.Lock()
		stale := time.Since(a.sub.lastCheck) > 2*time.Minute
		a.sub.mu.Unlock()
		if stale {
			a.mu.Lock()
			var pref []subscription.Server
			if p := a.profileByID(id); p != nil && p.Sub != nil {
				pref = p.Sub.Preferred(a.currentKind())
			}
			a.mu.Unlock()
			if len(pref) > 0 {
				a.logf("%s — проверяю остальные серверы", why)
				a.testServers(id, pref)
			}
		}
		a.mu.Lock()
		p := a.profileByID(id)
		next, ok := subscription.Server{}, false
		if p != nil && p.Sub != nil {
			next, ok = p.Sub.Pick(a.currentKind(), a.failedSet())
		}
		a.mu.Unlock()
		if ok {
			a.logf("%s — сервер «%s» не отвечает, переключаюсь на «%s»", why, curName, next.Name)
			a.notify("VPN: другой сервер", fmt.Sprintf("«%s» не отвечает, переключаюсь на «%s»", curName, next.Name), "")
			a.setStage(1)
			a.reconnect()
			return
		}
		a.logf("все серверы подписки перепробованы, связи нет")
		a.setStage(2)
		a.withTunnelDown(func() {
			if hasURL {
				if err := a.updateSubscription(id); err != nil {
					a.logf("свежий список серверов не получен: %v", err)
				}
			}
			a.sub.mu.Lock()
			a.sub.failed = nil
			a.sub.mu.Unlock()
			a.testServers(id, nil)
		})
	case stage == 2:
		if profiles > 1 && a.tryFailover() {
			a.setStage(0)
			return
		}
		a.setStage(5)
		msg := "Ни один сервер подписки не отвечает. Переподключения продолжаются; проверьте интернет или обновите подписку."
		a.alert.Store(&Alert{Kind: "server_down", Message: msg})
		a.notify("Серверы VPN не отвечают", msg, "")
	default:
		a.rec.mu.Lock()
		at := a.rec.actionAt
		a.rec.mu.Unlock()
		if time.Since(at) < health.RoundPause {
			return
		}
		a.logf("начинаю перебор серверов заново")
		a.sub.mu.Lock()
		a.sub.failed = nil
		a.sub.mu.Unlock()
		a.setStage(1)
		a.reconnect()
	}
}

// subNetChanged follows a switch between Wi-Fi, mobile internet and cable: when the new network
// has its own rule that the current server does not fit, the tunnel picks a server for it.
func (a *App) subNetChanged(kind string) {
	if kind == "" {
		return
	}
	a.sub.mu.Lock()
	was := a.sub.kind
	a.sub.mu.Unlock()
	if kind == was {
		return
	}
	a.mu.Lock()
	p := a.secrets.Active()
	if p == nil || p.Sub == nil {
		a.mu.Unlock()
		return
	}
	rule := p.Sub.Rule(kind)
	cur := subscription.Find(p.Sub.Servers, p.Sub.Current)
	keep := cur != nil && subscription.Prefers(rule, *cur)
	a.mu.Unlock()
	a.sub.mu.Lock()
	a.sub.kind = kind
	a.sub.failed = nil
	a.sub.mu.Unlock()
	if keep {
		return
	}
	a.logf("сеть сменилась на «%s» — выбираю сервер по правилу «%s»", subui.NetName(kind), subui.RuleName(rule, p.Sub.Servers))
	a.notify("VPN: смена сети", fmt.Sprintf("Сеть «%s» — выбираю подходящий сервер", subui.NetName(kind)), "")
	a.setStage(0)
	a.reconnect()
}

// ---- background updates and warnings -------------------------------------------------------

// subUpdater refreshes subscriptions as often as their providers ask (12 hours if they do not
// say) and warns about the end of the paid period or the traffic.
func (a *App) subUpdater() {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	first := time.After(90 * time.Second)
	for {
		select {
		case <-a.stop:
			return
		case <-first:
		case <-t.C:
		}
		type due struct{ id string }
		var list []due
		a.mu.Lock()
		for _, p := range a.secrets.Profiles {
			if p.Sub == nil || p.Sub.URL == "" {
				continue
			}
			every := time.Duration(p.Sub.Info.UpdateHours) * time.Hour
			if every <= 0 {
				every = 12 * time.Hour
			}
			if time.Since(p.Sub.Updated) >= every {
				list = append(list, due{p.ID})
			}
		}
		a.mu.Unlock()
		for _, d := range list {
			a.updateSubscription(d.id)
		}
		a.subAlerts()
	}
}

// subAlerts shows a banner when the active subscription is about to end or its traffic runs out.
func (a *App) subAlerts() {
	a.mu.Lock()
	p := a.secrets.Active()
	if p == nil || p.Sub == nil || !a.settings.CheckSubscription {
		a.mu.Unlock()
		return
	}
	in := p.Sub.Info
	a.mu.Unlock()
	msg := subui.Alert(in, expiryWarnBefore)
	if msg == "" {
		if al := a.alert.Load(); al != nil && al.Kind == "expiring" {
			a.alert.Store(nil)
		}
		return
	}
	if al := a.alert.Load(); al == nil || al.Message != msg {
		a.alert.Store(&Alert{Kind: "expiring", Message: msg})
		a.notify("Подписка VPN", msg, "")
	}
}

// ---- state for the UI ----------------------------------------------------------------------

// OpenSubLink opens the active subscription's support chat ("support") or web page ("web") in
// the browser. Only links the provider sent in the subscription headers are opened.
func (a *App) OpenSubLink(which string) error {
	a.mu.Lock()
	p := a.secrets.Active()
	link := ""
	if p != nil && p.Sub != nil {
		switch which {
		case "support":
			link = p.Sub.Info.SupportURL
		case "web":
			link = p.Sub.Info.WebPage
		}
	}
	a.mu.Unlock()
	if link == "" {
		return errors.New("у подписки нет такой ссылки")
	}
	wruntime.BrowserOpenURL(a.ctx, link)
	return nil
}
