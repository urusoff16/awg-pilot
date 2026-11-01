package main

import (
	"fmt"
	"sync"
	"time"

	"awgpilot/internal/health"
)

const (
	staleAfter        = health.StaleAfter
	subscriptionEvery = 6 * time.Hour
	expiryWarnBefore  = 3 * 24 * time.Hour
)

// recovery tracks escalation while the server stays unreachable (same steps as on Windows).
type recovery struct {
	mu           sync.Mutex
	stage        int       // recovery step reached (health.Plan); 0 = healthy
	actionAt     time.Time // last recovery step
	connected    bool
	pingOK       string // profile whose server has answered our ping
	failovers    int    // profiles switched to in this round
	netChangedAt time.Time
	noNet        bool // Android reports no network at all
}

func (a *App) setStage(s int) {
	a.rec.mu.Lock()
	a.rec.stage, a.rec.actionAt = s, time.Now()
	a.rec.mu.Unlock()
}

// monitor samples throughput every second and announces the first handshake.
func (a *App) monitor() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var lastSave time.Time
	var prevRx, prevTx uint64
	havePrev := false
	for {
		st := a.stats()
		now := time.Now()
		if st.Up {
			a.meter.Add(now, st.RxBytes, st.TxBytes)
			if havePrev && st.RxBytes >= prevRx && st.TxBytes >= prevTx {
				a.usage.Add(now, st.RxBytes-prevRx, st.TxBytes-prevTx)
			}
			prevRx, prevTx, havePrev = st.RxBytes, st.TxBytes, true
			if now.Sub(lastSave) >= time.Minute {
				lastSave = now
				if err := a.usage.Save(); err != nil {
					a.logf("статистика не сохранена: %v", err)
				}
			}
			a.mu.Lock()
			id := a.secrets.ActiveID
			a.mu.Unlock()
			a.rec.mu.Lock()
			if st.PingMs >= 0 {
				a.rec.pingOK = id
			}
			first := !st.LastHandshake.IsZero() && !a.rec.connected
			if !st.LastHandshake.IsZero() {
				a.rec.connected = true
			}
			a.rec.mu.Unlock()
			if first && a.announce.Swap(false) {
				a.notify("VPN подключён", "Сервер "+hostOf(st.Endpoint))
			}
		} else {
			havePrev = false
			a.meter.Reset()
			a.rec.mu.Lock()
			a.rec.connected = false
			a.rec.mu.Unlock()
		}
		select {
		case <-a.stop:
			return
		case <-t.C:
		}
	}
}

// watchdog restores a silent tunnel (see package health for the rules and the order of steps).
// API calls need no tunnel teardown here: the app itself is excluded from the VPN.
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
		st := a.stats()
		if !st.Up {
			continue
		}
		now := time.Now()
		a.rec.mu.Lock()
		in := health.Input{Now: now, Since: st.Since, LastHandshake: st.LastHandshake, PingFails: st.PingFails,
			PingWorks: a.rec.pingOK == activeID, NetChangedAt: a.rec.netChangedAt, ActionAt: a.rec.actionAt}
		stage, actionAt, noNet := a.rec.stage, a.rec.actionAt, a.rec.noNet
		moreProfiles := a.rec.failovers < profiles-1
		a.rec.mu.Unlock()

		dead, why := health.Dead(in)
		if !dead {
			if stage > 0 && health.Recovered(in) {
				a.rec.mu.Lock()
				a.rec.stage, a.rec.failovers = 0, 0
				a.rec.mu.Unlock()
				a.logf("соединение восстановлено")
				a.notify("Соединение восстановлено", "VPN снова работает")
				if al := a.alert.Load(); al != nil && al.Kind == "server_down" {
					a.alert.Store(nil)
				}
			}
			continue
		}
		if !enabled || noNet {
			continue // without any network there is nothing to reconnect over; wait for one
		}

		step := health.Plan(stage, hasKey, moreProfiles)
		// A server that never answered usually means the key in the config was revoked.
		if stage == 0 && st.LastHandshake.IsZero() && hasKey {
			step = health.RefreshConfig
		}
		switch step {
		case health.Reconnect:
			if stage == 0 {
				a.logf("%s — переподключаюсь", why)
				a.notify("Связь с сервером потеряна", "Переподключаюсь…")
			} else {
				a.logf("%s — переподключаюсь ещё раз", why)
			}
			a.setStage(stage + 1)
			a.reconnect()
		case health.RefreshConfig:
			a.logf("%s — запрашиваю у провайдера свежий конфиг", why)
			a.setStage(3)
			if err := a.RefreshConfig(); err != nil {
				a.logf("свежий конфиг не получен: %v", err)
			}
		case health.NextProfile:
			if a.tryFailover() {
				a.setStage(0)
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
					a.notify("Сервер VPN не отвечает", msg)
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

func (a *App) reconnect() {
	if err := a.reconnectIfUp(); err != nil {
		a.logf("переподключение не удалось: %v", err)
	}
}

// tryFailover switches to the next profile with a key or config and reconnects on it.
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

	a.mu.Lock()
	n, cur := len(a.secrets.Profiles), 0
	for i, p := range a.secrets.Profiles {
		if p.ID == a.secrets.ActiveID {
			cur = i
		}
	}
	name, needConfig, ok := "", false, false
	for i := 1; i < n; i++ {
		p := a.secrets.Profiles[(cur+i)%n]
		if p.RawConfig == "" && p.VPNKey == "" {
			continue
		}
		a.secrets.ActiveID = p.ID
		a.activateLocked()
		a.saveSecrets()
		name, needConfig, ok = p.Name, p.RawConfig == "", true
		break
	}
	a.mu.Unlock()
	if !ok {
		return false
	}
	a.logf("переключаюсь на профиль «%s»", name)
	a.notify("VPN: другой профиль", "Переключаюсь на профиль «"+name+"»")
	if needConfig {
		if err := a.DownloadConfig(false); err != nil {
			a.logf("конфиг для профиля «%s» не получен: %v", name, err)
		}
	}
	a.reconnect()
	return true
}

func (a *App) offerServerChange(reason string) {
	msg := "Сервер не отвечает даже после переподключения и обновления конфига. Можно запросить другой сервер (раз в сутки)."
	if reason != "" {
		msg = "Сервер не отвечает, а другой сервер получить не удалось: " + reason
	}
	a.alert.Store(&Alert{Kind: "server_down", Message: msg, Action: "change_server"})
	a.notify("Сервер VPN не отвечает", msg)
}

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
			a.notify("Подписка VPN", msg)
		}
	}
}
