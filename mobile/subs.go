package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"awgpilot/internal/health"
	"awgpilot/internal/netx"
	"awgpilot/internal/store"
	"awgpilot/internal/subscription"
	"awgpilot/internal/subui"
	"awgpilot/internal/xray"
)

// subState is what the core keeps about proxy subscriptions besides the saved profile.
type subState struct {
	mu      sync.Mutex
	failed  map[string]bool // servers that did not work in this recovery round
	kind    string          // network kind the tunnel was brought up for
	testing atomic.Bool
	// testingID is the profile whose servers the running check measures.
	testingID string
	// lastCheck is when servers were last checked for a recovery or a connect.
	lastCheck time.Time
	tested    atomic.Int32
	total     atomic.Int32
}

// proxyAddr is the VPN interface's own address for a subscription server: Xray's stack answers
// for every destination, so it only has to be an address nothing else uses.
const proxyAddr = "172.19.0.1/30"

// netKind is the phone's network as Android last reported it.
func (a *App) netKind() string {
	k, _ := a.kind.Load().(string)
	return k
}

func (a *App) subHeaders() subscription.Headers {
	a.mu.Lock()
	defer a.mu.Unlock()
	ua := a.settings.UserAgent
	if ua == "" {
		ua = "AWGPilot/" + appVersion + " (Android)"
	}
	return subscription.Headers{UserAgent: ua, HWID: a.settings.DeviceID, OS: "Android", OSVersion: fmt.Sprint(a.sdk), Model: "AWG Pilot"}
}

// addSubscription adds a subscription URL or server links as a profile and checks its servers.
func (a *App) addSubscription(text string) error {
	text = strings.TrimSpace(text)
	var info subscription.Info
	var servers []subscription.Server
	link := ""
	if low := strings.ToLower(text); strings.HasPrefix(low, "https://") || strings.HasPrefix(low, "http://") {
		link = text
		ctx, cancel := reqCtx()
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

func (a *App) UpdateSubscription() error {
	a.mu.Lock()
	id := a.secrets.ActiveID
	a.mu.Unlock()
	return a.updateSubscription(id)
}

func (a *App) updateSubscription(id string) error {
	a.mu.Lock()
	p := a.secrets.Find(id)
	if p == nil || p.Sub == nil {
		a.mu.Unlock()
		return errors.New("у профиля нет подписки")
	}
	link := p.Sub.URL
	a.mu.Unlock()
	if link == "" {
		return errors.New("серверы добавлены ссылками, а не подпиской — обновлять нечего")
	}
	ctx, cancel := reqCtx()
	info, servers, err := subscription.Fetch(ctx, netx.Client, link, a.subHeaders())
	cancel()
	a.mu.Lock()
	defer a.mu.Unlock()
	p = a.secrets.Find(id)
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
	return a.saveSecrets()
}

// TestServers checks every server of the active profile in the background.
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

// testServers measures the servers of profile id (only those in only, if given). The app is
// outside its own VPN, so the checks go straight over the phone's network even while connected.
func (a *App) testServers(id string, only []subscription.Server) {
	a.testServersCtx(context.Background(), id, only)
}

// testServersCtx is testServers that stops when ctx ends; unfinished servers keep their old checks.
func (a *App) testServersCtx(parent context.Context, id string, only []subscription.Server) {
	if !a.sub.testing.CompareAndSwap(false, true) {
		return
	}
	defer a.sub.testing.Store(false)
	a.mu.Lock()
	p := a.secrets.Find(id)
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
	a.sub.testingID = id
	a.sub.mu.Unlock()
	links := make([]string, len(list))
	for i, s := range list {
		links[i] = s.Link
	}
	var ok atomic.Int32
	// A bound on the whole check: a server that hangs must not keep "checking" on screen forever.
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	xray.TestAll(ctx, links, 8, 6*time.Second, func(i int, r xray.Result) {
		if r.Ms <= 0 && ctx.Err() != nil {
			return // cut short, not a failure of the server
		}
		if r.Ms > 0 {
			ok.Add(1)
		}
		a.mu.Lock()
		if p := a.secrets.Find(id); p != nil && p.Sub != nil {
			p.Sub.SetMeasure(list[i].ID, subscription.Measure{Ms: r.Ms, Err: r.Error, At: time.Now()})
		}
		a.mu.Unlock()
		a.sub.tested.Add(1)
	})
	a.mu.Lock()
	a.saveSecrets()
	a.mu.Unlock()
	a.logf("серверы «%s» проверены: отвечают %d из %d", name, ok.Load(), len(list))
}

// SetServerRule sets how servers are chosen on a kind of network; "" returns to the default.
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
	if kind == a.currentKind() {
		a.clearFailed()
		a.reconnectFresh()
	}
	return nil
}

// currentKind is the network kind rules are looked up for ("wired" when Android has not said).
func (a *App) currentKind() string {
	if k := a.netKind(); k != "" {
		return k
	}
	return subscription.NetWired
}

// UseServer connects through one server now and keeps it for the current kind of network.
func (a *App) UseServer(id string) error {
	if err := a.SetServerRule(a.currentKind(), "server:"+id); err != nil {
		return err
	}
	up := a.curTun() != nil
	if !up {
		return a.Connect()
	}
	return nil
}

// reconnectFresh brings the tunnel up again choosing the server anew, if it is up.
func (a *App) reconnectFresh() {
	a.mu.Lock()
	if p := a.secrets.Active(); p != nil && p.Sub != nil {
		p.Sub.Current = ""
	}
	a.mu.Unlock()
	if err := a.reconnectIfUp(); err != nil {
		a.logf("переподключение не удалось: %v", err)
	}
}

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

func (a *App) clearFailed() {
	a.sub.mu.Lock()
	a.sub.failed = nil
	a.sub.mu.Unlock()
}

// connectSubLocked (re)builds the VPN interface around a server of the subscription. Callers
// hold tunOp; a running tunnel is replaced in place, as for AmneziaWG.
func (a *App) connectSubLocked(id string) error {
	kind := a.currentKind()
	a.mu.Lock()
	p := a.secrets.Find(id)
	if p == nil || p.Sub == nil || len(p.Sub.Servers) == 0 {
		a.mu.Unlock()
		return errors.New("в подписке нет серверов: нажмите «Обновить подписку»")
	}
	rule := p.Sub.Rule(kind)
	need := subscription.NeedTest(p.Sub.Servers, rule, p.Sub.Measures, time.Now())
	preferred := p.Sub.Preferred(kind)
	profName := p.Name
	s := a.settings
	a.mu.Unlock()
	if need && len(preferred) > 0 && !a.sub.testing.Load() {
		a.logf("проверяю серверы для сети «%s»…", subui.NetName(kind))
		a.connectTest(id, preferred)
	}
	if a.connectCancelled() {
		return errAborted
	}

	dns := dnsServers(s)
	if len(dns) == 0 { // a subscription brings no DNS servers of its own
		for _, d := range dnsPresets["cloudflare"] {
			dns = append(dns, netip.MustParseAddr(d))
		}
	}
	plan := Plan{Session: "AWG Pilot — " + profName, MTU: 1500, AppMode: s.AppMode, Apps: s.AppPackages, Addresses: []string{proxyAddr}}
	if s.MTU > 0 {
		plan.MTU = s.MTU
	}
	for _, d := range dns {
		plan.DNS = append(plan.DNS, d.String())
	}
	rs := s
	if s.Split.RussiaByDomain() {
		rs.Split.BypassRussia = false // Xray sends Russian sites straight out by domain: no routes needed
	}
	if err := a.addRoutes(&plan, rs, dns, false, nil); err != nil {
		return err
	}
	dropV6 := s.BlockIPv6

	for attempt := 0; attempt < 5; attempt++ {
		if a.connectCancelled() {
			return errAborted
		}
		a.mu.Lock()
		p = a.secrets.Find(id)
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
		link, err := xray.ParseLink(srv.Link)
		if err != nil {
			a.markFailed(srv.ID)
			continue
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
		fd, err := a.plat.Establish(plan)
		if err != nil {
			if a.tun == nil {
				a.plat.Release()
			}
			a.logf("VPN не создан: %v", err)
			return err
		}
		if a.tun != nil {
			a.tun.Close()
			a.setTun(nil)
		}
		t, err := startProxy(fd, link, srv.ID, addr, plan.MTU, dropV6, a.flows, s.DirectDomains())
		if err != nil {
			a.plat.Release()
			a.logf("подключение через «%s» не удалось: %v", srv.Name, err)
			return err
		}
		a.setTun(t)
		a.meter.Reset()
		a.mu.Lock()
		if p := a.secrets.Find(id); p != nil && p.Sub != nil {
			p.Sub.Current = srv.ID
			a.saveSecrets()
		}
		a.mu.Unlock()
		a.sub.mu.Lock()
		a.sub.kind = kind
		a.sub.mu.Unlock()
		a.logf("туннель поднят через Xray: сервер %s (%s, %s), маршрутов %d (+%d исключений)", srv.Name, srv.Proto, link.Endpoint(), len(plan.Routes), len(plan.ExcludeRoutes))
		return nil
	}
	return errors.New("адреса серверов не находятся — проверьте интернет")
}

// SetNetKind is called by Android when the phone's default network changes kind ("wifi",
// "mobile", "wired"). With a subscription whose rule differs for the new network, the tunnel
// moves to a server for it.
func (a *App) SetNetKind(kind string) {
	if kind != subscription.NetWiFi && kind != subscription.NetMobile && kind != subscription.NetWired {
		kind = ""
	}
	if old := a.netKind(); old == kind {
		return
	}
	a.kind.Store(kind)
	if kind == "" {
		return
	}
	if a.auto != nil && a.auto.NetChanged(kind) {
		a.sub.mu.Lock()
		a.sub.kind, a.sub.failed = kind, nil
		a.sub.mu.Unlock()
		return // moved to the profile for this network
	}
	t, _ := a.curTun().(*proxyTunnel)
	if t == nil {
		return
	}
	a.sub.mu.Lock()
	was := a.sub.kind
	a.sub.mu.Unlock()
	if was == kind {
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
	servers := p.Sub.Servers
	a.mu.Unlock()
	a.sub.mu.Lock()
	a.sub.kind = kind
	a.sub.failed = nil
	a.sub.mu.Unlock()
	if keep {
		return
	}
	a.logf("сеть сменилась на «%s» — выбираю сервер по правилу «%s»", subui.NetName(kind), subui.RuleName(rule, servers))
	a.notify("VPN: смена сети", fmt.Sprintf("Сеть «%s» — выбираю подходящий сервер", subui.NetName(kind)))
	a.setStage(0)
	a.reconnect()
}

// subRecover is the watchdog's step for a subscription whose server stopped answering: one
// reconnect, then the other servers fastest first, then a fresh list and a full check, then other
// profiles, and again from the start.
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
		a.notify("Связь с сервером потеряна", "Переподключаюсь…")
		a.setStage(1)
		a.reconnect()
	case stage <= 1:
		if cur != nil {
			a.markFailed(cur.ID)
			a.mu.Lock()
			if p := a.secrets.Find(id); p != nil && p.Sub != nil {
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
			if p := a.secrets.Find(id); p != nil && p.Sub != nil {
				pref = p.Sub.Preferred(a.currentKind())
			}
			a.mu.Unlock()
			if len(pref) > 0 {
				a.logf("%s — проверяю остальные серверы", why)
				a.testServers(id, pref)
			}
		}
		a.mu.Lock()
		next, ok := subscription.Server{}, false
		if p := a.secrets.Find(id); p != nil && p.Sub != nil {
			next, ok = p.Sub.Pick(a.currentKind(), a.failedSet())
		}
		a.mu.Unlock()
		if ok {
			a.logf("%s — сервер «%s» не отвечает, переключаюсь на «%s»", why, curName, next.Name)
			a.notify("VPN: другой сервер", fmt.Sprintf("«%s» не отвечает, переключаюсь на «%s»", curName, next.Name))
			a.setStage(1)
			a.reconnect()
			return
		}
		a.logf("все серверы подписки перепробованы, связи нет")
		a.setStage(2)
		if hasURL {
			if err := a.updateSubscription(id); err != nil {
				a.logf("свежий список серверов не получен: %v", err)
			}
		}
		a.clearFailed()
		a.testServers(id, nil)
		a.reconnect()
	case stage == 2:
		if profiles > 1 && a.tryFailover() {
			a.setStage(0)
			return
		}
		a.setStage(5)
		msg := "Ни один сервер подписки не отвечает. Переподключения продолжаются; проверьте интернет или обновите подписку."
		a.alert.Store(&Alert{Kind: "server_down", Message: msg})
		a.notify("Серверы VPN не отвечают", msg)
	default:
		a.rec.mu.Lock()
		at := a.rec.actionAt
		a.rec.mu.Unlock()
		if time.Since(at) < health.RoundPause {
			return
		}
		a.logf("начинаю перебор серверов заново")
		a.clearFailed()
		a.setStage(1)
		a.reconnect()
	}
}

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
		var due []string
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
				due = append(due, p.ID)
			}
		}
		a.mu.Unlock()
		for _, id := range due {
			a.updateSubscription(id)
		}
		a.mu.Lock()
		p := a.secrets.Active()
		var in subscription.Info
		on := p != nil && p.Sub != nil && a.settings.CheckSubscription
		if on {
			in = p.Sub.Info
		}
		a.mu.Unlock()
		if !on {
			continue
		}
		msg := subui.Alert(in, expiryWarnBefore)
		if msg == "" {
			if al := a.alert.Load(); al != nil && al.Kind == "expiring" {
				a.alert.Store(nil)
			}
			continue
		}
		if al := a.alert.Load(); al == nil || al.Message != msg {
			a.alert.Store(&Alert{Kind: "expiring", Message: msg})
			a.notify("Подписка VPN", msg)
		}
	}
}

// SubLink returns the active subscription's support ("support") or web page ("web") link for
// Android to open.
func (a *App) SubLink(which string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.secrets.Active()
	if p != nil && p.Sub != nil {
		switch which {
		case "support":
			if p.Sub.Info.SupportURL != "" {
				return p.Sub.Info.SupportURL, nil
			}
		case "web":
			if p.Sub.Info.WebPage != "" {
				return p.Sub.Info.WebPage, nil
			}
		}
	}
	return "", errors.New("у подписки нет такой ссылки")
}

// errAborted ends a connect the user cancelled with "Disconnect".
var errAborted = errors.New("подключение отменено")

// connectTest checks servers before a connect: at most 15 seconds, and it stops at once when the
// user cancels the connect, so "Disconnect" never waits for a whole check.
func (a *App) connectTest(id string, list []subscription.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	go func() {
		t := time.NewTicker(150 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if a.connectCancelled() {
					cancel()
					return
				}
			}
		}
	}()
	a.testServersCtx(ctx, id, list)
}

func (a *App) connectCancelled() bool { return a.abort.Load() }

type tunBox struct{ t vpnTunnel }

// setTun replaces the running tunnel; callers hold tunOp.
func (a *App) setTun(t vpnTunnel) {
	a.tun = t
	a.tunBox.Store(tunBox{t})
}

// curTun is the running tunnel without waiting for tunOp (nil when down).
func (a *App) curTun() vpnTunnel {
	b, _ := a.tunBox.Load().(tunBox)
	return b.t
}
