package main

import (
	"context"
	"errors"
	"time"

	"awgpilot/internal/autopilot"
	"awgpilot/internal/awgprobe"
	"awgpilot/internal/i18n"
	"awgpilot/internal/store"
	"awgpilot/internal/subscription"
	"awgpilot/internal/xray"
)

// autoHost lets the shared autopilot drive the Android core.
type autoHost struct{ a *App }

func (h autoHost) Snapshot() autopilot.Snapshot {
	a := h.a
	st := a.stats()
	a.mu.Lock()
	s := autopilot.Snapshot{Settings: a.settings, ActiveID: a.secrets.ActiveID, Profiles: store.CloneProfiles(a.secrets.Profiles)}
	a.mu.Unlock()
	s.Kind = a.netKind()
	s.Up, s.PingMs = st.Up, st.PingMs
	s.Answering = st.Up && !st.LastHandshake.IsZero() && time.Since(st.LastHandshake) < staleAfter
	a.rec.mu.Lock()
	s.Recovering = a.rec.stage > 0
	a.rec.mu.Unlock()
	return s
}

func (h autoHost) SwitchProfile(id, why string) error {
	h.a.logf("%s", why)
	h.a.announce.Store(true)
	return h.a.SelectProfile(id)
}

// ProbeAWG needs no pinning: the app is outside its own VPN, so the probe goes straight out.
func (h autoHost) ProbeAWG(raw string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return awgprobe.Probe(ctx, raw, nil)
}

func (h autoHost) TestServers(id, kind string) {
	a := h.a
	if kind == "" {
		a.testServers(id, nil)
		return
	}
	a.mu.Lock()
	p := a.secrets.Find(id)
	if p == nil || p.Sub == nil {
		a.mu.Unlock()
		return
	}
	pref := p.Sub.Preferred(kind)
	a.mu.Unlock()
	if len(pref) > 0 {
		a.testServers(id, pref)
	}
}

func (h autoHost) ReconnectServer(why string) {
	h.a.logf("%s", why)
	h.a.reconnectFresh()
}

func (h autoHost) Logf(format string, args ...any) { h.a.logf(format, args...) }
func (h autoHost) Notify(title, body string)       { h.a.notify(title, body) }

// CheckProfile checks a profile now (the one in use by its live delay) and returns the result.
func (a *App) CheckProfile(id string) (autopilot.Check, error) {
	if a.auto == nil {
		return autopilot.Check{}, errors.New("проверка недоступна")
	}
	return a.auto.CheckProfile(id)
}

// TestProfileServers checks every server of a subscription profile in the background.
func (a *App) TestProfileServers(id string) error {
	a.mu.Lock()
	p := a.secrets.Find(id)
	ok := p != nil && p.Sub != nil && len(p.Sub.Servers) > 0
	a.mu.Unlock()
	if !ok {
		return errors.New("у профиля нет серверов подписки")
	}
	if a.sub.testing.Load() {
		return errors.New("проверка серверов уже идёт")
	}
	go a.testServers(id, nil)
	return nil
}

// PingServer checks one server of a subscription profile now and keeps the result.
func (a *App) PingServer(profileID, serverID string) (subscription.Measure, error) {
	a.mu.Lock()
	link := ""
	if p := a.secrets.Find(profileID); p != nil && p.Sub != nil {
		if srv := subscription.Find(p.Sub.Servers, serverID); srv != nil {
			link = srv.Link
		}
	}
	a.mu.Unlock()
	if link == "" {
		return subscription.Measure{}, errors.New("сервер не найден в подписке")
	}
	r := a.pingLink(link)
	m := subscription.Measure{Ms: r.Ms, Err: r.Error, At: time.Now()}
	a.mu.Lock()
	if p := a.secrets.Find(profileID); p != nil && p.Sub != nil {
		p.Sub.SetMeasure(serverID, m)
		a.saveSecrets()
	}
	a.mu.Unlock()
	m.Err = i18n.T(m.Err)
	return m, nil
}

// ConnectServer connects through one server of a subscription profile (making the profile active)
// and keeps that server for the current kind of network, as a choice by hand.
func (a *App) ConnectServer(profileID, serverID string) error {
	kind := a.currentKind()
	a.mu.Lock()
	p := a.secrets.Find(profileID)
	if p == nil || p.Sub == nil || subscription.Find(p.Sub.Servers, serverID) == nil {
		a.mu.Unlock()
		return errors.New("сервер не найден в подписке")
	}
	if p.Sub.Rules == nil {
		p.Sub.Rules = map[string]string{}
	}
	p.Sub.Rules[kind] = "server:" + serverID
	p.Sub.Current = ""
	active := a.secrets.ActiveID == profileID
	err := a.saveSecrets()
	a.mu.Unlock()
	if err != nil {
		return err
	}
	if a.auto != nil {
		a.auto.Manual()
	}
	a.sub.mu.Lock()
	a.sub.failed = nil
	a.sub.mu.Unlock()
	up := a.tunnelUp()
	switch {
	case !active:
		if err := a.selectProfileFor(profileID); err != nil {
			return err
		}
	case up:
		a.reconnectFresh()
	}
	if !up {
		return a.Connect()
	}
	return nil
}

// pingLink measures one server straight over the phone's network (the app is outside its VPN).
func (a *App) pingLink(link string) xray.Result {
	return xray.Test(context.Background(), link, 6*time.Second)
}

func (a *App) tunnelUp() bool {
	a.tunOp.Lock()
	defer a.tunOp.Unlock()
	return a.tun != nil
}

func (a *App) selectProfileFor(id string) error { return a.SelectProfile(id) }
