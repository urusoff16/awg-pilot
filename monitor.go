package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"awgpilot/internal/health"
	"awgpilot/internal/i18n"
	"awgpilot/internal/tunnel"
)

const (
	staleAfter        = 190 * time.Second // WireGuard re-handshakes every 2 minutes while traffic flows
	subscriptionEvery = 6 * time.Hour
	expiryWarnBefore  = 3 * 24 * time.Hour
	categoryServer    = "server_down"
	actionChange      = "change_server"
	actionOpen        = "open"
)

// recovery tracks escalation while the server stays unreachable.
type recovery struct {
	mu              sync.Mutex
	stage           int       // recovery step reached (health.Plan); 0 = healthy
	actionAt        time.Time // last recovery step
	connected       bool      // a handshake has been seen on the current tunnel
	pingOK          string    // profile whose server has answered our ping
	netChanges      uint64    // the service's network switch counter, last seen
	netSeen         bool
	netChangedAt    time.Time
	failovers       int  // profiles switched to in this round
	blockedNotified bool // the "VPN dropped, internet blocked" toast was shown
	lastBlockedTry  time.Time
}

// ---- notifications ------------------------------------------------------------------------

func (a *App) initNotifications() {
	if err := wruntime.InitializeNotifications(a.ctx); err != nil {
		a.logf("уведомления Windows недоступны: %v", err)
		return
	}
	err := wruntime.RegisterNotificationCategory(a.ctx, wruntime.NotificationCategory{
		ID: categoryServer,
		Actions: []wruntime.NotificationAction{
			{ID: actionChange, Title: i18n.T("Запросить другой сервер")},
			{ID: actionOpen, Title: i18n.T("Открыть")},
		},
	})
	if err != nil {
		a.logf("не удалось зарегистрировать кнопки уведомлений: %v", err)
	}
	wruntime.OnNotificationResponse(a.ctx, func(r wruntime.NotificationResult) {
		if r.Error != nil {
			return
		}
		switch r.Response.ActionIdentifier {
		case actionChange:
			go func() {
				if err := a.RequestOtherServer(); err != nil {
					a.notify("Другой сервер не получен", err.Error(), "")
					a.ShowWindow()
				}
			}()
		default:
			a.ShowWindow()
		}
	})
}

// notify shows a Windows toast when notifications are enabled. category "" = no buttons.
func (a *App) notify(title, body, category string) {
	a.mu.Lock()
	on := a.settings.Notifications
	a.mu.Unlock()
	if !on || a.ctx == nil {
		return
	}
	opts := wruntime.NotificationOptions{ID: fmt.Sprintf("awgpilot-%d", time.Now().UnixNano()), Title: i18n.T(title), Body: i18n.T(body), CategoryID: category}
	var err error
	if category != "" {
		err = wruntime.SendNotificationWithActions(a.ctx, opts)
	} else {
		err = wruntime.SendNotification(a.ctx, opts)
	}
	if err != nil {
		a.logf("уведомление не показано: %v", err)
	}
}

// DismissAlert hides the current problem banner.
func (a *App) DismissAlert() { a.alert.Store(nil) }

// ---- live monitor -------------------------------------------------------------------------

// monitor samples throughput every second, pings through the tunnel and mirrors the state
// in the tray icon. It also announces the first handshake after a connect or recovery.
func (a *App) monitor() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var lastSave time.Time
	var prevRx, prevTx uint64
	havePrev := false
	svcLogNext := -1 // -1: first read starts at the service's current end, skipping old history
	for {
		a.pullServiceLogs(&svcLogNext)
		st := a.tun.Stats()
		now := time.Now()
		if st.Up {
			a.meter.Add(now, st.RxBytes, st.TxBytes)
			// per-day statistics; a counter drop means a new tunnel, so restart the baseline
			if havePrev && st.RxBytes >= prevRx && st.TxBytes >= prevTx && a.usage != nil {
				a.usage.Add(now, st.RxBytes-prevRx, st.TxBytes-prevTx)
			}
			prevRx, prevTx, havePrev = st.RxBytes, st.TxBytes, true
			if a.usage != nil && now.Sub(lastSave) >= time.Minute {
				lastSave = now
				if err := a.usage.Save(); err != nil {
					a.logf("статистика не сохранена: %v", err)
				}
			}
			a.pingMs.Store(st.PingMs) // measured by the service through the tunnel
			if st.PingMs >= 0 {
				a.mu.Lock()
				id := a.secrets.ActiveID
				a.mu.Unlock()
				a.rec.mu.Lock()
				a.rec.pingOK = id
				a.rec.mu.Unlock()
			}
			if !st.LastHandshake.IsZero() {
				a.rec.mu.Lock()
				first := !a.rec.connected
				a.rec.connected = true
				a.rec.mu.Unlock()
				if first && a.announce.Swap(false) {
					a.notify("VPN подключён", "Сервер "+hostOf(st.Endpoint), "")
				}
			}
		} else {
			havePrev = false
			a.meter.Reset()
			a.pingMs.Store(-1)
			a.rec.mu.Lock()
			a.rec.connected = false
			a.rec.mu.Unlock()
		}
		a.updateTray(st)
		select {
		case <-a.stop:
			return
		case <-t.C:
		}
	}
}

// pullServiceLogs copies new service log lines ("15:04:05 text") into the window's journal.
func (a *App) pullServiceLogs(next *int) {
	since := *next
	if since < 0 {
		since = 1 << 30 // ask for nothing; the reply tells where the log currently ends
	}
	lines, n, err := a.tun.c.Logs(since)
	if err != nil {
		return
	}
	if *next >= 0 {
		for _, l := range lines {
			if len(l) > 9 {
				a.logs.add(l[:9] + "служба: " + l[9:])
			} else {
				a.logs.add(l)
			}
		}
	}
	*next = n
}

func hostOf(endpoint string) string {
	host, _, _ := strings.Cut(endpoint, ":")
	if host == "" {
		return "—"
	}
	return host
}

func (a *App) updateTray(st tunnel.Stats) {
	if a.tray == nil {
		return
	}
	up, endpoint, hs := st.Up, st.Endpoint, st.LastHandshake
	tip := "AWG Pilot — отключено"
	if st.Blocked {
		tip = "AWG Pilot — интернет заблокирован\nVPN оборвался, идёт восстановление"
	}
	if up {
		tip = "AWG Pilot — подключено\nСервер " + hostOf(endpoint)
		if last, _ := a.meter.Snapshot(); last.T != 0 {
			tip += fmt.Sprintf("\n↓ %s/с  ↑ %s/с", humanBytes(last.Rx), humanBytes(last.Tx))
		}
		if hs.IsZero() || time.Since(hs) > staleAfter {
			tip = "AWG Pilot — нет ответа от сервера"
		}
	}
	a.tray.SetState(up, i18n.T(tip))
}

func humanBytes(v float64) string {
	units := []string{"Б", "КБ", "МБ", "ГБ"}
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f %s", v, units[i])
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

// ---- auto recovery ------------------------------------------------------------------------

// watchdog restores a broken VPN (see package health for when a tunnel counts as dead and
// the order of the steps). While the tunnel is down because the lockdown caught a drop, it
// keeps trying to connect.
func (a *App) watchdog() {
	t := time.NewTicker(health.Tick)
	defer t.Stop()
	for {
		select {
		case <-a.stop:
			return
		case <-t.C:
		}
		a.mu.Lock()
		enabled, autoChange, hasKey := a.settings.Reconnect, a.settings.AutoChangeServer, a.key != nil
		activeID, profiles := a.secrets.ActiveID, len(a.secrets.Profiles)
		a.mu.Unlock()
		st := a.tun.Stats()
		if !st.Up {
			if st.Blocked && enabled {
				a.retryWhileBlocked()
			}
			continue
		}
		now := time.Now()
		a.rec.mu.Lock()
		if st.NetChanges != a.rec.netChanges {
			if a.rec.netSeen { // the first reading is only the baseline
				a.rec.netChangedAt = now
				a.logf("сеть сменилась — проверяю связь с сервером")
			}
			a.rec.netChanges = st.NetChanges
		}
		a.rec.netSeen = true
		in := health.Input{Now: now, Since: st.Since, LastHandshake: st.LastHandshake, PingFails: st.PingFails,
			PingWorks: a.rec.pingOK == activeID, NetChangedAt: a.rec.netChangedAt, ActionAt: a.rec.actionAt}
		stage, actionAt := a.rec.stage, a.rec.actionAt
		moreProfiles := a.rec.failovers < profiles-1
		a.rec.mu.Unlock()

		dead, why := health.Dead(in)
		if !dead {
			if stage > 0 && health.Recovered(in) {
				a.rec.mu.Lock()
				a.rec.stage, a.rec.failovers, a.rec.blockedNotified = 0, 0, false
				a.rec.mu.Unlock()
				a.logf("соединение восстановлено")
				a.notify("Соединение восстановлено", "VPN снова работает", "")
				if al := a.alert.Load(); al != nil && al.Kind == "server_down" {
					a.alert.Store(nil)
				}
			}
			continue
		}
		if !enabled {
			continue
		}
		if st.NoNetwork {
			continue // no Wi-Fi or cable at all: wait for a network instead of burning through steps
		}

		step := health.Plan(stage, hasKey, moreProfiles)
		// A server that has never answered on this tunnel usually means the config's key was
		// revoked (the provider issues a new one when another client downloads a config).
		if stage == 0 && st.LastHandshake.IsZero() && hasKey {
			step = health.RefreshConfig
		}
		switch step {
		case health.Reconnect:
			if stage == 0 {
				a.logf("%s — переподключаюсь", why)
				a.notify("Связь с сервером потеряна", "Переподключаюсь…", "")
			} else {
				a.logf("%s — переподключаюсь ещё раз", why)
			}
			a.setStage(stage + 1)
			a.reconnect()
		case health.RefreshConfig:
			a.logf("%s — запрашиваю у провайдера свежий конфиг", why)
			a.setStage(3)
			// The dead tunnel holds the default route, so talk to the API with it down.
			a.withTunnelDown(func() {
				if err := a.DownloadConfig(false); err != nil {
					a.logf("свежий конфиг не получен: %v", err)
				}
			})
		case health.NextProfile:
			if a.tryFailover() {
				a.setStage(0) // start over on the next profile
			} else {
				a.setStage(4)
			}
		case health.OtherServer:
			a.setStage(5)
			if autoChange {
				a.logf("сервер так и не ответил — автоматически запрашиваю другой сервер")
				if err := a.RequestOtherServer(); err != nil {
					a.logf("другой сервер не получен: %v", err)
					a.offerServerChange(err.Error())
				}
			} else {
				a.offerServerChange("")
			}
		case health.Pause:
			if stage < 5 {
				a.setStage(5)
				if !hasKey && profiles <= 1 {
					msg := "Сервер не отвечает. Без ключа vpn:// приложение не может обновить конфиг или запросить другой сервер. Переподключения продолжаются."
					a.alert.Store(&Alert{Kind: "server_down", Message: msg})
					a.notify("Сервер VPN не отвечает", msg, "")
				}
				continue
			}
			if now.Sub(actionAt) < health.RoundPause {
				continue
			}
			a.logf("все шаги пройдены, связи нет — начинаю заново")
			a.rec.mu.Lock()
			a.rec.failovers = 0
			a.rec.mu.Unlock()
			a.setStage(1)
			a.reconnect()
		}
	}
}

// tryFailover switches to the next profile that has a key or a config, with the tunnel down,
// and reconnects on it. It returns false once every other profile has been tried in this
// escalation.
func (a *App) tryFailover() bool {
	a.mu.Lock()
	others := len(a.secrets.Profiles) - 1
	a.mu.Unlock()
	a.rec.mu.Lock()
	if a.rec.failovers >= others {
		a.rec.mu.Unlock()
		return false
	}
	a.rec.failovers++
	a.rec.mu.Unlock()

	switched := false
	a.withTunnelDown(func() {
		name, needConfig, ok := a.selectNextProfile()
		if !ok {
			return
		}
		switched = true
		a.logf("переключаюсь на профиль «%s»", name)
		a.notify("VPN: другой профиль", "Переключаюсь на профиль «"+name+"»", "")
		if needConfig {
			if err := a.DownloadConfig(false); err != nil {
				a.logf("конфиг для профиля «%s» не получен: %v", name, err)
			}
		}
	})
	return switched
}

// selectNextProfile makes the next usable profile (after the active one, wrapping around)
// active. needConfig reports that it has a key but no config yet.
func (a *App) selectNextProfile() (name string, needConfig, ok bool) {
	a.mu.Lock()
	defer func() {
		a.mu.Unlock()
		if ok {
			a.trayProfiles()
		}
	}()
	n := len(a.secrets.Profiles)
	cur := 0
	for i, p := range a.secrets.Profiles {
		if p.ID == a.secrets.ActiveID {
			cur = i
		}
	}
	for i := 1; i < n; i++ {
		p := a.secrets.Profiles[(cur+i)%n]
		if p.RawConfig == "" && p.VPNKey == "" {
			continue
		}
		a.secrets.ActiveID = p.ID
		a.activateLocked()
		a.saveSecrets()
		return p.Name, p.RawConfig == "", true
	}
	return "", false, false
}

// retryWhileBlocked runs while the internet is closed after a drop: try the active profile
// again, then the next ones. It runs at most every 30 seconds.
func (a *App) retryWhileBlocked() {
	a.rec.mu.Lock()
	if time.Since(a.rec.lastBlockedTry) < 30*time.Second {
		a.rec.mu.Unlock()
		return
	}
	a.rec.lastBlockedTry = time.Now()
	first := !a.rec.blockedNotified
	a.rec.blockedNotified = true
	a.rec.mu.Unlock()
	if first {
		a.logf("VPN оборвался — интернет заблокирован, восстанавливаю подключение")
		a.notify("VPN оборвался", "Интернет заблокирован, пока VPN не восстановится. Восстанавливаю подключение…", "")
	}
	a.wanted.Store(true) // the internet is closed because the user wanted the VPN on
	a.tunOp.Lock()
	err := a.connectLocked()
	a.tunOp.Unlock()
	if err == nil {
		a.announce.Store(true)
		return
	}
	a.logf("подключение не восстановлено: %v", err)
	a.tryFailover()
}

func (a *App) setStage(s int) {
	a.rec.mu.Lock()
	a.rec.stage, a.rec.actionAt = s, time.Now()
	a.rec.mu.Unlock()
}

// reconnect restarts the tunnel; an armed lockdown keeps the internet closed in between.
func (a *App) reconnect() {
	a.tunOp.Lock()
	defer a.tunOp.Unlock()
	a.disconnectLocked(false)
	if err := a.connectLocked(); err != nil {
		a.logf("переподключение не удалось: %v", err)
	}
}

// withTunnelDown runs f with the tunnel stopped (so API calls do not go into a dead tunnel)
// and brings the tunnel back afterwards if it was running. An armed lockdown stays in place;
// this program itself is still allowed through it.
func (a *App) withTunnelDown(f func()) {
	a.tunOp.Lock()
	wasUp := a.tun.IsUp()
	if wasUp {
		a.disconnectLocked(false)
	}
	a.tunOp.Unlock()
	f()
	if !wasUp {
		return
	}
	a.tunOp.Lock()
	defer a.tunOp.Unlock()
	// The user may have pressed "Disconnect" while f talked to the provider.
	if !a.wanted.Load() || a.tun.IsUp() {
		return
	}
	if err := a.connectLocked(); err != nil {
		a.logf("переподключение не удалось: %v", err)
	}
}

func (a *App) offerServerChange(reason string) {
	msg := "Сервер не отвечает даже после переподключения и обновления конфига. Можно запросить другой сервер (раз в сутки)."
	if reason != "" {
		msg = "Сервер не отвечает, а другой сервер получить не удалось: " + reason
	}
	a.alert.Store(&Alert{Kind: "server_down", Message: msg, Action: actionChange})
	a.notify("Сервер VPN не отвечает", msg, categoryServer)
}

// RequestOtherServer asks the provider for another server (limited to once a day) and brings the
// tunnel back on the new server if it was running. The tunnel is down during the API calls,
// because the old server may be the one that is broken.
func (a *App) RequestOtherServer() error {
	var result error
	snap, err := a.snapshot() // one profile for the check, the change and the new config
	if err != nil {
		return err
	}
	a.withTunnelDown(func() {
		ok, err := a.checkChangeFor(snap)
		if err != nil {
			result = err
			return
		}
		if !ok {
			result = fmt.Errorf("другой сервер сейчас запросить нельзя — провайдер разрешает это раз в сутки")
			return
		}
		result = a.changeServerFor(snap)
	})
	if result != nil {
		return result
	}
	a.alert.Store(nil)
	a.setStage(0)
	a.announce.Store(true)
	a.notify("Получен другой сервер", "Конфиг обновлён", "")
	return nil
}

// ---- subscription -------------------------------------------------------------------------

// subscriptionWatcher refreshes the subscription status periodically and warns before expiry.
func (a *App) subscriptionWatcher() {
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	var lastWarn time.Time
	for {
		select {
		case <-a.stop:
			return
		case <-timer.C:
		}
		timer.Reset(subscriptionEvery)
		a.mu.Lock()
		enabled, hasKey := a.settings.CheckSubscription, a.key != nil
		a.mu.Unlock()
		if !enabled || !hasKey {
			continue
		}
		req, err := a.RefreshAccount()
		if err != nil {
			continue
		}
		until, err := time.Parse(time.RFC3339Nano, req.PaidUntil)
		if err != nil {
			continue
		}
		left := time.Until(until)
		if left > expiryWarnBefore {
			if al := a.alert.Load(); al != nil && al.Kind == "expiring" {
				a.alert.Store(nil)
			}
			continue
		}
		msg := fmt.Sprintf("Подписка заканчивается %s", until.Local().Format("02.01.2006 в 15:04"))
		if left <= 0 {
			msg = "Подписка закончилась — продлите её в Telegram-боте провайдера"
		}
		a.alert.Store(&Alert{Kind: "expiring", Message: msg})
		if time.Since(lastWarn) > 20*time.Hour {
			lastWarn = time.Now()
			a.notify("Подписка VPN", msg, "")
		}
	}
}
