// Command mobile is the Android core of AWG Pilot, built as libawgpilot.so. The Java side owns
// the VpnService and the WebView; everything else (profiles, provider API, routing, the
// amneziawg-go device, recovery) lives here and mirrors the Windows app.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"awgpilot/internal/awgconf"
	"awgpilot/internal/flows"
	"awgpilot/internal/geoip"
	"awgpilot/internal/i18n"
	"awgpilot/internal/keyapi"
	"awgpilot/internal/netstat"
	"awgpilot/internal/netx"
	"awgpilot/internal/remotecfg"
	"awgpilot/internal/routing"
	"awgpilot/internal/speedtest"
	"awgpilot/internal/store"
	"awgpilot/internal/usage"
	"awgpilot/internal/vpnkey"
)

const appVersion = "0.9.1"

var dnsPresets = map[string][]string{
	"cloudflare": {"1.1.1.1", "1.0.0.1"},
	"quad9":      {"9.9.9.9", "149.112.112.112"},
	"adguard":    {"94.140.14.14", "94.140.15.15"},
}

// blackholeV6 is given to the interface so Android routes IPv6 into the tunnel, where the device
// drops it (no peer accepts IPv6): apps fall back to IPv4 inside the VPN instead of leaking.
const blackholeV6 = "fd8a:7c1f:4e2d::2/128"

// Platform is implemented by the Java side.
type Platform interface {
	// Establish (re)builds the VpnService interface for plan and returns its file descriptor.
	Establish(plan Plan) (int, error)
	// Release closes the VPN interface and stops the service.
	Release()
	// Notify shows a notification when notifications are enabled.
	Notify(title, body string)
	// Owner names the app (package) behind a connection of the tunnel, "" when unknown.
	Owner(proto int, local string, lport int, remote string, rport int) string
}

// Plan is what VpnService.Builder needs.
type Plan struct {
	Session       string   `json:"session"`
	Addresses     []string `json:"addresses"`
	DNS           []string `json:"dns"`
	MTU           int      `json:"mtu"`
	Routes        []string `json:"routes"`
	ExcludeRoutes []string `json:"exclude_routes"` // Android 13+ only
	AppMode       string   `json:"app_mode"`       // "all", "include", "exclude"
	Apps          []string `json:"apps"`
}

// Stats mirror the Windows tunnel.Stats JSON the UI reads.
type Stats struct {
	Up            bool      `json:"up"`
	Endpoint      string    `json:"endpoint"`
	LastHandshake time.Time `json:"last_handshake"`
	RxBytes       uint64    `json:"rx_bytes"`
	TxBytes       uint64    `json:"tx_bytes"`
	Since         time.Time `json:"since"`
	KillSwitch    bool      `json:"kill_switch"`
	IPv6Blocked   bool      `json:"ipv6_blocked"`
	Blocked       bool      `json:"blocked"`
	PingMs        int64     `json:"ping_ms"`
	PingFails     int       `json:"ping_fails"`
}

type App struct {
	plat  Platform
	sdk   int          // Android API level
	trace func(string) // mirrors the journal (logcat on Android)

	mu        sync.Mutex
	st        *store.Store
	settings  store.Settings
	secrets   store.Secrets
	key       *vpnkey.Key
	remote    *remotecfg.Config
	account   *keyapi.Request
	countries []keyapi.Country
	warnings  []string
	deviceSrc string

	tunOp sync.Mutex // serializes connect/disconnect
	tun   *tunnel
	logs  logRing
	stop  chan struct{}

	meter        *netstat.Meter
	announce     atomic.Bool
	alert        atomic.Pointer[Alert]
	rec          recovery
	usage        *usage.Store
	flows        *flows.Recorder // per-app traffic log
	sysLang      string          // Android's language, from Java
	russiaStatus string
	speed        speedtest.Tracker

	secretsBlocked bool // secrets.bin is unreadable and could not be moved aside: never overwrite it
}

// ---- startup ------------------------------------------------------------------------------

func newApp(plat Platform, dir string, sdk int, trace func(string)) (*App, error) {
	a := &App{plat: plat, sdk: sdk, trace: trace, stop: make(chan struct{}), meter: netstat.NewMeter(120)}
	st, err := store.OpenDir(dir)
	if err != nil {
		return nil, err
	}
	a.st = st
	a.settings, err = st.LoadSettings()
	i18n.SetLanguage(i18n.Resolve(a.settings.Language, a.sysLang))
	if err != nil {
		a.logf("настройки повреждены, использую значения по умолчанию: %v", err)
	}
	if a.settings.AppMode == "" {
		a.settings.AppMode = "all"
	}
	a.secrets, err = st.LoadSecrets()
	switch {
	case err == nil:
	case len(store.SecretKey) == 0:
		// The Keystore did not hand over the key (possibly for a while): keep the file as it is;
		// saving is refused anyway (store.RequireKey), so nothing can overwrite it.
		a.secrets = store.Secrets{}
		a.logf("ключ шифрования Android Keystore недоступен — сохранённые профили не загружены, файл не тронут")
	default:
		a.secrets = store.Secrets{}
		name, qerr := st.QuarantineSecrets()
		if qerr != nil && !errors.Is(qerr, os.ErrNotExist) {
			a.secretsBlocked = true
			a.logf("не удалось расшифровать сохранённые ключи и конфиги (%v), а отложить файл не вышло (%v): изменения профилей сохраняться не будут", err, qerr)
		} else {
			a.logf("не удалось расшифровать сохранённые ключи и конфиги (%v); файл сохранён как %s", err, name)
		}
	}
	a.activateLocked()
	a.usage = usage.Open(filepath.Join(dir, "usage.json"))
	a.flows = flows.New(filepath.Join(dir, "traffic"), func(proto uint8, local netip.Addr, lport uint16, remote netip.Addr, rport uint16) string {
		return plat.Owner(int(proto), local.String(), int(lport), remote.String(), int(rport))
	})
	go a.flows.Run(a.stop)
	a.ensureDeviceID()
	a.logf("AWG Pilot %s для Android (API %d) запущен", appVersion, sdk)
	go a.monitor()
	go a.watchdog()
	go a.subscriptionWatcher()
	return a, nil
}

func (a *App) activateLocked() {
	a.key, a.account, a.countries, a.remote = nil, nil, nil, nil
	if p := a.secrets.Active(); p != nil && p.VPNKey != "" {
		a.key, _ = vpnkey.Decode(p.VPNKey)
	}
	a.alert.Store(nil)
}

func (a *App) ensureDeviceID() {
	if a.settings.DeviceID != "" {
		a.deviceSrc = "saved"
		return
	}
	a.settings.DeviceID, a.deviceSrc = newDeviceID(), "generated"
	a.saveSettings()
	a.logf("создан ID устройства AWG Pilot")
}

func newDeviceID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func (a *App) NewDeviceID() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.settings.DeviceID, a.deviceSrc = newDeviceID(), "generated"
	a.saveSettings()
	a.logf("создан новый ID устройства")
	return nil
}

func (a *App) saveSettings() {
	if err := a.st.SaveSettings(a.settings); err != nil {
		a.logf("не удалось сохранить настройки: %v", err)
	}
}

func (a *App) saveSecrets() error {
	if a.secretsBlocked {
		return errors.New("сохранённые ключи не читаются, поэтому изменения профилей не сохраняются — перезапустите приложение")
	}
	if err := a.st.SaveSecrets(a.secrets); err != nil {
		return fmt.Errorf("не удалось сохранить ключ и конфиг: %w", err)
	}
	return nil
}

func (a *App) client() (*keyapi.Client, error) {
	if a.key == nil {
		return nil, errors.New("сначала добавьте ключ vpn:// в разделе «Профили и ключи»")
	}
	base := a.key.APIEndpoint
	if a.remote != nil && strings.HasPrefix(a.remote.APIBaseURL, "https://") {
		base = a.remote.APIBaseURL
	}
	ua := a.settings.UserAgent
	if ua == "" {
		ua = "AWGPilot/" + appVersion + " (Android)"
	}
	return keyapi.New(base, a.settings.DeviceID, ua), nil
}

func reqCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// ---- state for the UI ---------------------------------------------------------------------

type State struct {
	Platform     string                 `json:"platform"`
	HasKey       bool                   `json:"has_key"`
	MaskedKey    string                 `json:"masked_key"`
	APIEndpoint  string                 `json:"api_endpoint"`
	HasConfig    bool                   `json:"has_config"`
	ConfigFor    string                 `json:"config_for"`
	ConfigInfo   *ConfigInfo            `json:"config_info"`
	Profiles     []ProfileInfo          `json:"profiles"`
	ActiveID     string                 `json:"active_id"`
	Settings     store.Settings         `json:"settings"`
	DeviceSource string                 `json:"device_source"`
	Account      *keyapi.Request        `json:"account"`
	Countries    []keyapi.Country       `json:"countries"`
	Connections  []remotecfg.Connection `json:"connections"`
	Tunnel       Stats                  `json:"tunnel"`
	Warnings     []string               `json:"warnings"`
	ServiceOK    bool                   `json:"service_ok"`
	Version      string                 `json:"version"`
	Speed        SpeedInfo              `json:"speed"`
	Alert        *Alert                 `json:"alert"`
	Usage        usage.Summary          `json:"usage"`
	RussiaList   string                 `json:"russia_list"`
	SDK          int                    `json:"sdk"`
	SpeedTest    speedtest.State        `json:"speedtest"`
	Lang         string                 `json:"lang"` // interface language in effect: "ru" or "en"
}

type ProfileInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	HasKey    bool   `json:"has_key"`
	MaskedKey string `json:"masked_key"`
	Endpoint  string `json:"endpoint"`
	HasConfig bool   `json:"has_config"`
	ConfigFor string `json:"config_for"`
}

type SpeedInfo struct {
	RxBps   float64         `json:"rx_bps"`
	TxBps   float64         `json:"tx_bps"`
	PingMs  int64           `json:"ping_ms"`
	History []netstat.Point `json:"history"`
}

type Alert struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Action  string `json:"action"`
}

type ConfigInfo struct {
	Endpoint string   `json:"endpoint"`
	Address  []string `json:"address"`
	DNS      []string `json:"dns"`
	MTU      int      `json:"mtu"`
	AWG      string   `json:"awg"`
}

func (a *App) stats() Stats {
	a.tunOp.Lock()
	t := a.tun
	a.tunOp.Unlock()
	if t == nil {
		return Stats{PingMs: -1}
	}
	return t.Stats()
}

func (a *App) GetState() State {
	st := a.stats()
	a.mu.Lock()
	defer a.mu.Unlock()
	s := State{
		Platform: "android", HasKey: a.key != nil, ActiveID: a.secrets.ActiveID, Settings: a.settings,
		DeviceSource: a.deviceSrc, Account: a.account, Countries: a.countries, Tunnel: st, Warnings: a.warnings,
		ServiceOK: true, Version: appVersion, RussiaList: a.russiaStatus, SDK: a.sdk,
	}
	for _, p := range a.secrets.Profiles {
		pi := ProfileInfo{ID: p.ID, Name: p.Name, HasConfig: p.RawConfig != "", ConfigFor: p.ConfigFor}
		if k, err := vpnkey.Decode(p.VPNKey); err == nil {
			pi.HasKey, pi.MaskedKey, pi.Endpoint = true, k.Masked(), k.APIEndpoint
		}
		s.Profiles = append(s.Profiles, pi)
	}
	s.Usage = a.usage.Summary(time.Now())
	s.Settings.DeviceID = ""
	last, hist := a.meter.Snapshot()
	s.Speed = SpeedInfo{RxBps: last.Rx, TxBps: last.Tx, PingMs: st.PingMs, History: hist}
	if !st.Up {
		s.Speed = SpeedInfo{PingMs: -1}
	}
	if al := a.alert.Load(); al != nil {
		c := *al
		c.Message = i18n.T(c.Message)
		s.Alert = &c
	}
	s.SpeedTest = a.speed.Snapshot()
	s.SpeedTest.Error = i18n.T(s.SpeedTest.Error)
	s.Lang = i18n.Lang()
	s.RussiaList = i18n.T(s.RussiaList)
	for i := range s.Profiles {
		s.Profiles[i].ConfigFor = i18n.T(s.Profiles[i].ConfigFor)
	}
	if len(s.Warnings) > 0 {
		w := make([]string, len(s.Warnings))
		for i, x := range s.Warnings {
			w[i] = i18n.T(x)
		}
		s.Warnings = w
	}
	if a.key != nil {
		s.MaskedKey, s.APIEndpoint = a.key.Masked(), a.key.APIEndpoint
	}
	if a.remote != nil {
		s.Connections = a.remote.Connections
	}
	if p := a.secrets.Active(); p != nil && p.RawConfig != "" {
		s.HasConfig, s.ConfigFor = true, p.ConfigFor
		if c, err := awgconf.Parse(p.RawConfig); err == nil {
			ci := &ConfigInfo{MTU: c.MTU, Endpoint: c.Peer.Endpoint, AWG: c.Summary()}
			for _, ad := range c.Addresses {
				ci.Address = append(ci.Address, ad.String())
			}
			for _, d := range c.DNS {
				ci.DNS = append(ci.DNS, d.String())
			}
			s.ConfigInfo = ci
		}
	}
	return s
}

// Brief is the small status the service polls for its notification and the quick tile.
type Brief struct {
	Up      bool    `json:"up"`
	Healthy bool    `json:"healthy"`
	Title   string  `json:"title"`
	RxBps   float64 `json:"rx_bps"`
	TxBps   float64 `json:"tx_bps"`
	PingMs  int64   `json:"ping_ms"`
	Server  string  `json:"server"`
	Profile string  `json:"profile"`
	Notify  bool    `json:"notify"` // show the status notification
}

func (a *App) Brief() Brief {
	st := a.stats()
	last, _ := a.meter.Snapshot()
	b := Brief{Up: st.Up, RxBps: last.Rx, TxBps: last.Tx, PingMs: st.PingMs, Server: hostOf(st.Endpoint)}
	a.mu.Lock()
	if p := a.secrets.Active(); p != nil {
		b.Profile = p.Name
	}
	b.Notify = a.settings.Notifications
	a.mu.Unlock()
	b.Healthy = st.Up && !st.LastHandshake.IsZero() && time.Since(st.LastHandshake) < staleAfter
	switch {
	case b.Healthy:
		b.Title = "VPN подключён"
	case st.Up:
		b.Title = "Подключение…"
	default:
		b.Title = "VPN отключён"
	}
	b.Title = i18n.T(b.Title)
	return b
}

func (a *App) GetLogs() []string {
	lines := a.logs.Lines()
	if i18n.English() {
		for i, l := range lines {
			lines[i] = i18n.T(l)
		}
	}
	return lines
}

// SetLanguage switches the interface language: "" follows Android, "ru" or "en".
func (a *App) SetLanguage(lang string) {
	a.mu.Lock()
	a.settings.Language = lang
	a.saveSettings()
	sys := a.sysLang
	a.mu.Unlock()
	i18n.SetLanguage(i18n.Resolve(lang, sys))
}

// SetSystemLanguage takes Android's language (Java passes it right after the core starts).
func (a *App) SetSystemLanguage(tag string) string {
	a.mu.Lock()
	a.sysLang = tag
	setting := a.settings.Language
	a.mu.Unlock()
	i18n.SetLanguage(i18n.Resolve(setting, tag))
	return i18n.Lang()
}

// SpeedTest measures download and upload through the tunnel (Cloudflare speed test servers).
func (a *App) SpeedTest() error {
	a.tunOp.Lock()
	t := a.tun
	a.tunOp.Unlock()
	if t == nil {
		return errors.New("сначала подключите VPN")
	}
	st, err := t.pt.startStack(t.mtu)
	if err != nil {
		return fmt.Errorf("тест скорости не запустился: %w", err)
	}
	if !a.speed.Start(st.Dial, func(r speedtest.Result, err error) {
		t.pt.stopStack(st)
		if err != nil {
			a.logf("тест скорости не удался: %v", err)
			return
		}
		a.logf("тест скорости через VPN (IP %s, %s): загрузка %.1f Мбит/с, отдача %.1f Мбит/с", r.IP, r.Loc, r.DownMbps, r.UpMbps)
	}) {
		t.pt.stopStack(st)
		return errors.New("тест скорости уже идёт")
	}
	a.logf("тест скорости через VPN запущен")
	return nil
}

func (a *App) ResetUsage() error {
	a.logf("статистика трафика очищена")
	return a.usage.Reset()
}

func (a *App) DismissAlert() { a.alert.Store(nil) }

// ---- profiles -----------------------------------------------------------------------------

func (a *App) addProfileLocked(p store.Profile) {
	p.ID = store.NewID()
	if strings.TrimSpace(p.Name) == "" {
		p.Name = fmt.Sprintf("Профиль %d", len(a.secrets.Profiles)+1)
	}
	a.secrets.Profiles = append(a.secrets.Profiles, p)
	a.secrets.ActiveID = p.ID
	a.activateLocked()
}

// switchProfile runs change and reconnects on the new active profile if the VPN was on.
func (a *App) switchProfile(change func() error) error {
	a.tunOp.Lock()
	defer a.tunOp.Unlock()
	a.mu.Lock()
	before := a.secrets.ActiveID
	a.mu.Unlock()
	wasUp := a.tun != nil
	if err := change(); err != nil {
		return err
	}
	a.mu.Lock()
	switched := a.secrets.ActiveID != before
	p := a.secrets.Active()
	canConnect := p != nil && p.RawConfig != ""
	a.mu.Unlock()
	if !wasUp || !switched {
		return nil
	}
	if canConnect {
		return a.connectLocked()
	}
	a.disconnectLocked()
	return nil
}

// nativeKey imports an AmneziaVPN share key (self-hosted server) as a config profile.
func nativeKey(raw string, keyErr error) (*vpnkey.Native, error) {
	n, err := vpnkey.DecodeNative(raw)
	if err != nil {
		return nil, keyErr // neither format: report the provider-key error
	}
	if n.Config == "" {
		return nil, fmt.Errorf("в ключе AmneziaVPN нет протокола AmneziaWG (есть: %s) — в AmneziaVPN поделитесь доступом в формате AmneziaWG", strings.Join(n.Others, ", "))
	}
	return n, nil
}

func (a *App) AddKey(raw string) error {
	k, err := vpnkey.Decode(raw)
	if err != nil {
		n, nerr := nativeKey(raw, err)
		if nerr != nil {
			return nerr
		}
		return a.ImportConfigText(n.Config, n.Name)
	}
	return a.switchProfile(func() error {
		a.mu.Lock()
		defer a.mu.Unlock()
		for _, p := range a.secrets.Profiles {
			if old, err := vpnkey.Decode(p.VPNKey); err == nil && old.APIKey == k.APIKey {
				a.secrets.ActiveID = p.ID
				a.activateLocked()
				a.logf("ключ %s уже сохранён — выбран профиль «%s»", k.Masked(), p.Name)
				return a.saveSecrets()
			}
		}
		a.addProfileLocked(store.Profile{VPNKey: strings.TrimSpace(raw)})
		a.logf("добавлен профиль с ключом %s (%s)", k.Masked(), k.APIEndpoint)
		return a.saveSecrets()
	})
}

// ImportConfigText stores a wg-quick/AmneziaWG config as a new active profile.
func (a *App) ImportConfigText(text, name string) error {
	if t := strings.TrimSpace(text); strings.HasPrefix(strings.ToLower(t), "vpn://") {
		return a.AddKey(t) // a .vpn key file from AmneziaVPN or the provider
	}
	if _, err := awgconf.Parse(text); err != nil {
		return fmt.Errorf("это не конфиг AmneziaWG/WireGuard: %w", err)
	}
	origin := "импортирован"
	if name != "" {
		origin = "файл " + name
		name = strings.TrimSuffix(name, filepath.Ext(name))
	}
	return a.switchProfile(func() error {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.addProfileLocked(store.Profile{Name: name, RawConfig: text, ConfigFor: origin})
		a.logf("конфиг импортирован как профиль «%s»", a.secrets.Active().Name)
		return a.saveSecrets()
	})
}

// ExportConfig returns the active profile's name and config text.
func (a *App) ExportConfig() (map[string]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.secrets.Active()
	if p == nil || p.RawConfig == "" {
		return nil, errors.New("у активного профиля ещё нет конфига")
	}
	return map[string]string{"name": p.Name, "text": p.RawConfig}, nil
}

func (a *App) SelectProfile(id string) error {
	return a.switchProfile(func() error {
		a.mu.Lock()
		defer a.mu.Unlock()
		p := a.secrets.Find(id)
		if p == nil {
			return errors.New("профиль не найден")
		}
		if a.secrets.ActiveID == id {
			return nil
		}
		a.secrets.ActiveID = id
		a.activateLocked()
		a.logf("выбран профиль «%s»", p.Name)
		return a.saveSecrets()
	})
}

func (a *App) RenameProfile(id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("имя профиля не может быть пустым")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.secrets.Find(id)
	if p == nil {
		return errors.New("профиль не найден")
	}
	p.Name = name
	return a.saveSecrets()
}

func (a *App) DeleteProfile(id string) error {
	return a.switchProfile(func() error {
		a.mu.Lock()
		defer a.mu.Unlock()
		p := a.secrets.Find(id)
		if p == nil {
			return errors.New("профиль не найден")
		}
		name, wasActive := p.Name, a.secrets.ActiveID == id
		a.secrets.Remove(id)
		if wasActive {
			a.activateLocked()
		}
		a.logf("профиль «%s» удалён", name)
		return a.saveSecrets()
	})
}

// ---- provider API -------------------------------------------------------------------------
// The app is excluded from its own VPN, so these requests always go over the phone's network,
// even while the tunnel is up but the server is dead.

type apiSnapshot struct {
	c         *keyapi.Client
	apiKey    string
	settings  store.Settings
	remote    *remotecfg.Config
	profileID string
}

func (a *App) snapshot() (apiSnapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, err := a.client()
	if err != nil {
		return apiSnapshot{}, err
	}
	return apiSnapshot{c: c, apiKey: a.key.APIKey, settings: a.settings, remote: a.remote, profileID: a.secrets.ActiveID}, nil
}

func (a *App) RefreshAccount() (*keyapi.Request, error) {
	a.mu.Lock()
	needRemote := a.remote == nil && a.key != nil && strings.HasPrefix(a.key.APIEndpoint, remotecfg.GatewayAPI)
	pid := a.secrets.ActiveID // answers are kept only for the profile they were asked for
	a.mu.Unlock()
	if needRemote {
		ctx, cancel := reqCtx()
		rc, err := remotecfg.Fetch(ctx, remotecfg.GatewayConfigURL)
		cancel()
		if err != nil {
			a.logf("настройки провайдера не загрузились: %v", err)
		} else {
			a.mu.Lock()
			if a.secrets.ActiveID == pid {
				a.remote = rc
			}
			a.mu.Unlock()
		}
	}
	snap, err := a.snapshot()
	if err != nil {
		return nil, err
	}
	ctx, cancel := reqCtx()
	defer cancel()
	req, err := snap.c.GetRequest(ctx, snap.apiKey)
	if err != nil {
		a.logf("запрос статуса подписки не удался: %v", err)
		return nil, err
	}
	countries, cerr := snap.c.GetCountries(ctx)
	a.mu.Lock()
	if a.secrets.ActiveID != snap.profileID {
		a.mu.Unlock()
		return nil, errors.New("профиль переключён, пока шёл запрос — обновите статус ещё раз")
	}
	a.account = req
	if cerr == nil {
		a.countries = countries
	}
	a.mu.Unlock()
	a.logf("подписка: статус %s, оплачено до %s, свободно устройств %v из %v", req.SimplifiedStatus, req.PaidUntil, req.NumberOfAvailableKeys, req.KeyLimit)
	return req, nil
}

func (a *App) DownloadConfig(forceRebind bool) error {
	snap, err := a.snapshot()
	if err != nil {
		return err
	}
	return a.downloadFor(snap, forceRebind)
}

// downloadFor downloads a config into the profile snap was taken for, even if the user switched
// profiles meanwhile.
func (a *App) downloadFor(snap apiSnapshot, forceRebind bool) error {
	opts := keyapi.DownloadOptions{ISOCountryCode: snap.settings.Country, KeyType: snap.settings.KeyType, ForceUpdateDevice: forceRebind}
	if snap.remote != nil {
		if conn := snap.remote.Connection(snap.settings.KeyType); conn != nil {
			opts.RelativePath = conn.RelativePath
		}
	}
	ctx, cancel := reqCtx()
	defer cancel()
	raw, err := snap.c.DownloadAWGKey(ctx, snap.apiKey, opts)
	if err != nil {
		a.logf("получение конфига не удалось: %v", err)
		return err
	}
	if _, err := awgconf.Parse(raw); err != nil {
		return fmt.Errorf("сервер прислал непригодный конфиг: %w", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.secrets.Find(snap.profileID)
	if p == nil {
		return errors.New("профиль удалён, пока загружался конфиг")
	}
	p.RawConfig = raw
	p.ConfigFor = strings.Trim(snap.settings.Country+" "+snap.settings.KeyType, " ")
	if p.ConfigFor == "" {
		p.ConfigFor = "сервер по умолчанию"
	}
	a.logf("получен новый конфиг для профиля «%s» (%s)", p.Name, p.ConfigFor)
	return a.saveSecrets()
}

// RefreshConfig downloads a fresh config and reconnects on it if the VPN is on.
func (a *App) RefreshConfig() error {
	if err := a.DownloadConfig(false); err != nil {
		return err
	}
	return a.reconnectIfUp()
}

func (a *App) checkChangeServer(snap apiSnapshot) (bool, error) {
	ctx, cancel := reqCtx()
	defer cancel()
	return snap.c.CheckChangeServer(ctx, snap.apiKey, snap.settings.KeyType, snap.settings.Country)
}

// changeServer changes the server and downloads the new config for the same profile.
func (a *App) changeServer(snap apiSnapshot) error {
	ctx, cancel := reqCtx()
	err := snap.c.ChangeServer(ctx, snap.apiKey, snap.settings.KeyType, snap.settings.Country)
	cancel()
	if err != nil {
		a.logf("смена сервера не удалась: %v", err)
		return err
	}
	a.logf("сервер сменён")
	return a.downloadFor(snap, false)
}

// RequestOtherServer asks the provider for another server (once a day) and reconnects on it.
func (a *App) RequestOtherServer() error {
	snap, err := a.snapshot() // one profile for the check, the change and the new config
	if err != nil {
		return err
	}
	ok, err := a.checkChangeServer(snap)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("другой сервер сейчас запросить нельзя — провайдер разрешает это раз в сутки")
	}
	if err := a.changeServer(snap); err != nil {
		return err
	}
	a.alert.Store(nil)
	a.setStage(0)
	a.announce.Store(true)
	a.notify("Получен другой сервер", "Конфиг обновлён")
	return a.reconnectIfUp()
}

// ---- settings -----------------------------------------------------------------------------

func (a *App) SaveSettings(s store.Settings) error {
	if s.Split.Mode == "" {
		s.Split.Mode = routing.ModeFull
	}
	if s.MTU != 0 && (s.MTU < 1100 || s.MTU > 1500) {
		return errors.New("MTU должен быть от 1100 до 1500 или 0 (взять из конфига)")
	}
	for _, d := range s.CustomDNS {
		if _, err := netip.ParseAddr(strings.TrimSpace(d)); err != nil {
			return fmt.Errorf("неверный адрес DNS-сервера %q", d)
		}
	}
	if s.DNSPreset == "custom" && len(s.CustomDNS) == 0 {
		return errors.New("укажите хотя бы один свой DNS-сервер или выберите другой вариант")
	}
	switch s.AppMode {
	case "", "all":
		s.AppMode = "all"
	case "include", "exclude":
		if s.AppMode == "include" && len(s.AppPackages) == 0 {
			return errors.New("выберите хотя бы одно приложение, которое пойдёт через VPN")
		}
	default:
		return fmt.Errorf("неизвестный режим приложений %q", s.AppMode)
	}
	a.mu.Lock()
	s.DeviceID = a.settings.DeviceID
	a.settings = s
	a.saveSettings()
	a.mu.Unlock()
	return nil
}

// ---- tunnel -------------------------------------------------------------------------------

func dnsServers(s store.Settings) []netip.Addr {
	var list []string
	switch s.DNSPreset {
	case "custom":
		list = s.CustomDNS
	case "provider", "":
		return nil
	default:
		list = dnsPresets[s.DNSPreset]
	}
	var out []netip.Addr
	for _, s := range list {
		if ad, err := netip.ParseAddr(strings.TrimSpace(s)); err == nil {
			out = append(out, ad)
		}
	}
	return out
}

var lanPrefixes = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("169.254.0.0/16")}

// buildPlan turns the active config and settings into a VpnService plan and the UAPI config.
func (a *App) buildPlan() (Plan, *awgconf.Config, string, error) {
	a.mu.Lock()
	var raw, profName string
	if p := a.secrets.Active(); p != nil {
		raw, profName = p.RawConfig, p.Name
	}
	s := a.settings
	a.mu.Unlock()
	if raw == "" {
		return Plan{}, nil, "", errors.New("у профиля нет конфига: нажмите «Обновить конфиг» или импортируйте .conf")
	}
	cfg, err := awgconf.Parse(raw)
	if err != nil {
		return Plan{}, nil, "", fmt.Errorf("сохранённый конфиг повреждён: %w", err)
	}
	plan := Plan{Session: "AWG Pilot — " + profName, MTU: cfg.MTU, AppMode: s.AppMode, Apps: s.AppPackages}
	if s.MTU > 0 {
		plan.MTU = s.MTU
	}
	if plan.MTU == 0 {
		plan.MTU = 1280
	}
	hasV6 := false
	for _, ad := range cfg.Addresses {
		plan.Addresses = append(plan.Addresses, ad.String())
		hasV6 = hasV6 || ad.Addr().Is6()
	}
	dns := dnsServers(s)
	if len(dns) == 0 {
		dns = cfg.DNS
	}
	for _, d := range dns {
		plan.DNS = append(plan.DNS, d.String())
	}

	// Routes: everything not excluded goes into the tunnel; the tunnel DNS always does.
	ctx, cancel := reqCtx()
	entries, warns := resolveEntries(ctx, s.Split.Entries)
	cancel()
	var fixed, bypass []netip.Prefix
	var keep []netip.Prefix
	for _, d := range dns {
		if d.Is4() {
			keep = append(keep, netip.PrefixFrom(d, 32))
		}
	}
	mode := s.Split.Mode
	if mode == routing.ModeInclude {
		if len(entries) == 0 {
			return Plan{}, nil, "", errors.New("в режиме «только список» нужен хотя бы один адрес или домен")
		}
		for _, p := range routing.Merge(append(entries, keep...)) {
			plan.Routes = append(plan.Routes, p.String())
		}
	} else {
		if mode == routing.ModeExclude {
			fixed = append(fixed, entries...)
		}
		if s.Split.BypassLAN {
			fixed = append(fixed, lanPrefixes...)
		}
		if s.Split.BypassRussia {
			ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
			list, err := geoip.Get(ctx, filepath.Join(a.st.Dir(), "ru-ipv4.txt"))
			cancel()
			switch {
			case list == nil:
				return Plan{}, nil, "", fmt.Errorf("не удалось получить список российских сетей: %w", err)
			case err != nil:
				a.logf("список российских сетей: %v", err)
			}
			var share float64
			bypass, share = fitBypass(fixed, list.Prefixes, keep, a.sdk >= 33)
			a.mu.Lock()
			a.russiaStatus = fmt.Sprintf("%d сетей, обновлён %s", len(list.Prefixes), list.FetchedAt.Local().Format("02.01.2006"))
			a.mu.Unlock()
			a.logf("Россия мимо VPN: напрямую %d крупнейших блоков, %.0f%% российских адресов (лимит маршрутов Android)", len(bypass), share*100)
		}
		excl := routing.Subtract(append(fixed, bypass...), keep) // the tunnel DNS stays inside
		switch {
		case len(excl) == 0:
			plan.Routes = []string{"0.0.0.0/0"}
		case a.sdk >= 33:
			plan.Routes = []string{"0.0.0.0/0"}
			for _, p := range routing.Merge(excl) {
				plan.ExcludeRoutes = append(plan.ExcludeRoutes, p.String())
			}
		default:
			for _, p := range routing.Merge(append(routing.Subtract(allIPv4, excl), keep...)) {
				plan.Routes = append(plan.Routes, p.String())
			}
		}
	}
	switch {
	case hasV6:
		// A provider-issued IPv6 address: IPv6 goes through the tunnel as the config says
		// (the split plan covers IPv4 only), never straight to the phone's network.
		v6Default := false
		for _, p := range cfg.Peer.AllowedIPs {
			if p.Addr().Is6() {
				plan.Routes = append(plan.Routes, p.String())
				v6Default = v6Default || p.Bits() == 0
			}
		}
		if s.BlockIPv6 && !v6Default {
			plan.Routes = append(plan.Routes, "::/0") // the rest of IPv6 is dropped inside the tunnel
		}
	case s.BlockIPv6:
		plan.Addresses = append(plan.Addresses, blackholeV6)
		plan.Routes = append(plan.Routes, "::/0")
	}
	a.mu.Lock()
	a.warnings = warns
	a.mu.Unlock()
	for _, w := range warns {
		a.logf("раздельный туннель: %s", w)
	}
	return plan, cfg, profName, nil
}

// hasIPv6 reports whether the provider issued an IPv6 interface address.
func hasIPv6(cfg *awgconf.Config) bool {
	for _, ad := range cfg.Addresses {
		if ad.Addr().Is6() {
			return true
		}
	}
	return false
}

func resolveEntries(ctx context.Context, entries []string) ([]netip.Prefix, []string) {
	var out []netip.Prefix
	var warns []string
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if p, err := netip.ParsePrefix(e); err == nil && p.Addr().Is4() {
			out = append(out, p.Masked())
			continue
		}
		if ad, err := netip.ParseAddr(e); err == nil {
			if ad.Is4() {
				out = append(out, netip.PrefixFrom(ad, 32))
			}
			continue
		}
		ips, err := netx.Resolver.LookupNetIP(ctx, "ip4", e)
		if err != nil || len(ips) == 0 {
			warns = append(warns, fmt.Sprintf("домен %s не разрешился", e))
			continue
		}
		for _, ip := range ips {
			out = append(out, netip.PrefixFrom(ip.Unmap(), 32))
		}
	}
	return out, warns
}

// resolveEndpoint turns host:port into ip:port (the device needs an IP).
func resolveEndpoint(ep string) (string, error) {
	host, port, err := net.SplitHostPort(ep)
	if err != nil {
		return "", fmt.Errorf("неверный адрес сервера %q", ep)
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return ep, nil
	}
	ctx, cancel := reqCtx()
	defer cancel()
	ips, err := netx.Resolver.LookupNetIP(ctx, "ip4", host)
	if err != nil || len(ips) == 0 {
		return "", fmt.Errorf("не удалось найти адрес сервера %s", host)
	}
	return net.JoinHostPort(ips[0].Unmap().String(), port), nil
}

func (a *App) Connect() error {
	a.tunOp.Lock()
	defer a.tunOp.Unlock()
	return a.connectLocked()
}

// connectLocked (re)builds the VPN interface and starts a fresh device on it. A running tunnel
// is replaced in place, so the VPN key icon never drops while reconnecting.
func (a *App) connectLocked() error {
	plan, cfg, profName, err := a.buildPlan()
	if err != nil {
		return err
	}
	endpoint, err := resolveEndpoint(cfg.Peer.Endpoint)
	if err != nil {
		return err
	}
	allowed := []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")} // the OS routes decide what enters
	for _, p := range cfg.Peer.AllowedIPs {
		if p.Addr().Is6() && hasIPv6(cfg) {
			allowed = append(allowed, p)
		}
	}
	uapi := cfg.UAPI(endpoint, allowed)
	a.logf("подключаюсь: профиль «%s»", profName)
	// Establish replaces the previous interface atomically; only then stop the old device.
	fd, err := a.plat.Establish(plan)
	if err != nil {
		// The old interface (if any) is still in place: keep it rather than drop the VPN.
		if a.tun == nil {
			a.plat.Release()
		}
		a.logf("VPN не создан: %v", err)
		return err
	}
	if a.tun != nil {
		a.tun.Close()
		a.tun = nil
	}
	var pingTo netip.Addr
	if len(plan.DNS) > 0 {
		pingTo, _ = netip.ParseAddr(plan.DNS[0])
	}
	var src netip.Addr
	for _, ad := range cfg.Addresses {
		if ad.Addr().Is4() {
			src = ad.Addr()
			break
		}
	}
	t, err := startTunnel(fd, uapi, plan.MTU, src, pingTo, a.flows, a.logf)
	if err != nil {
		a.plat.Release()
		a.logf("подключение не удалось: %v", err)
		return err
	}
	t.endpoint, t.v6Blocked, t.mtu = endpoint, strings.Contains(strings.Join(plan.Routes, ","), "::/0"), plan.MTU
	a.tun = t
	a.meter.Reset()
	a.logf("туннель поднят: сервер %s, маршрутов %d (+%d исключений)", endpoint, len(plan.Routes), len(plan.ExcludeRoutes))
	return nil
}

func (a *App) Disconnect() {
	a.tunOp.Lock()
	defer a.tunOp.Unlock()
	a.disconnectLocked()
}

func (a *App) disconnectLocked() {
	if a.tun != nil {
		a.tun.Close()
		a.tun = nil
		a.logf("отключено")
	}
	a.plat.Release()
}

// Revoked is called when Android takes the VPN away (another VPN app started, or the user
// turned it off in the system settings).
func (a *App) Revoked() {
	a.tunOp.Lock()
	defer a.tunOp.Unlock()
	if a.tun != nil {
		a.tun.Close()
		a.tun = nil
		a.logf("система отключила VPN (включён другой VPN или он выключен в настройках)")
	}
}

// NetworkChanged rebinds the sockets after Wi-Fi/mobile switches so the handshake recovers fast;
// the watchdog then checks the link more strictly for a minute and reconnects if it stays dead.
func (a *App) NetworkChanged() {
	a.rec.mu.Lock()
	a.rec.netChangedAt, a.rec.noNet = time.Now(), false
	a.rec.mu.Unlock()
	a.tunOp.Lock()
	defer a.tunOp.Unlock()
	if a.tun != nil {
		a.logf("сеть сменилась — проверяю связь с сервером")
		a.tun.Rebind()
	}
}

// SetTrafficLog switches the per-app traffic log on or off.
func (a *App) SetTrafficLog(on bool) error {
	if err := a.flows.SetEnabled(on); err != nil {
		return fmt.Errorf("журнал трафика не переключён: %w", err)
	}
	a.logf("журнал трафика по приложениям %s", map[bool]string{true: "включён", false: "выключен"}[on])
	return nil
}

// NetworkLost pauses recovery while the phone has no network at all.
func (a *App) NetworkLost() {
	a.rec.mu.Lock()
	a.rec.noNet = true
	a.rec.mu.Unlock()
}

func (a *App) reconnectIfUp() error {
	a.tunOp.Lock()
	defer a.tunOp.Unlock()
	if a.tun == nil {
		return nil
	}
	return a.connectLocked()
}

// ---- log ring -----------------------------------------------------------------------------

type logRing struct {
	mu    sync.Mutex
	lines []string
}

func (r *logRing) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, s)
	if len(r.lines) > 500 {
		r.lines = r.lines[len(r.lines)-500:]
	}
}

func (r *logRing) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

func (a *App) logf(format string, args ...any) {
	line := time.Now().Format("15:04:05 ") + fmt.Sprintf(format, args...)
	a.logs.add(line)
	if a.trace != nil {
		a.trace(line)
	}
}

func (a *App) notify(title, body string) {
	a.mu.Lock()
	on := a.settings.Notifications
	a.mu.Unlock()
	if on {
		a.plat.Notify(i18n.T(title), i18n.T(body))
	}
}

func hostOf(endpoint string) string {
	host, _, _ := strings.Cut(endpoint, ":")
	return host
}

// ---- JSON dispatch ------------------------------------------------------------------------

// Call runs one UI method. args is a JSON array of the method's arguments; the reply is
// {"ok":true,"result":...} or {"ok":false,"error":"..."}.
func (a *App) Call(method, args string) string {
	var in []json.RawMessage
	if args != "" {
		if err := json.Unmarshal([]byte(args), &in); err != nil {
			return reply(nil, fmt.Errorf("неверные аргументы: %w", err))
		}
	}
	str := func(i int) string {
		var s string
		if i < len(in) {
			json.Unmarshal(in[i], &s)
		}
		return s
	}
	boolean := func(i int) bool {
		var b bool
		if i < len(in) {
			json.Unmarshal(in[i], &b)
		}
		return b
	}
	switch method {
	case "GetState":
		return reply(a.GetState(), nil)
	case "Brief":
		return reply(a.Brief(), nil)
	case "GetLogs":
		return reply(a.GetLogs(), nil)
	case "SpeedTest":
		return reply(nil, a.SpeedTest())
	case "ResetUsage":
		return reply(nil, a.ResetUsage())
	case "TrafficLog":
		rep := a.flows.Report(str(0))
		for i := range rep.Rows {
			rep.Rows[i].App, rep.Rows[i].Host = i18n.T(rep.Rows[i].App), i18n.T(rep.Rows[i].Host)
		}
		return reply(rep, nil)
	case "Translate":
		return reply(i18n.T(str(0)), nil)
	case "SetLanguage":
		a.SetLanguage(str(0))
		return reply(i18n.Lang(), nil)
	case "SetSystemLanguage":
		return reply(a.SetSystemLanguage(str(0)), nil)
	case "Lang":
		return reply(i18n.Lang(), nil)
	case "SetSkin":
		a.mu.Lock()
		a.settings.Skin = str(0)
		a.saveSettings()
		a.mu.Unlock()
		return reply(nil, nil)
	case "SetTrafficLog":
		return reply(nil, a.SetTrafficLog(boolean(0)))
	case "ClearTrafficLog":
		a.logf("журнал трафика по приложениям очищен")
		return reply(nil, a.flows.Clear())
	case "DismissAlert":
		a.DismissAlert()
		return reply(nil, nil)
	case "AddKey":
		return reply(nil, a.AddKey(str(0)))
	case "ImportConfigText":
		return reply(nil, a.ImportConfigText(str(0), str(1)))
	case "ExportConfig":
		return reply(a.ExportConfig())
	case "SelectProfile":
		return reply(nil, a.SelectProfile(str(0)))
	case "RenameProfile":
		return reply(nil, a.RenameProfile(str(0), str(1)))
	case "DeleteProfile":
		return reply(nil, a.DeleteProfile(str(0)))
	case "RefreshAccount":
		return reply(a.RefreshAccount())
	case "DownloadConfig":
		if boolean(0) {
			if err := a.DownloadConfig(true); err != nil {
				return reply(nil, err)
			}
			return reply(nil, a.reconnectIfUp())
		}
		return reply(nil, a.RefreshConfig())
	case "RequestOtherServer":
		return reply(nil, a.RequestOtherServer())
	case "SaveSettings":
		var s store.Settings
		if len(in) == 0 || json.Unmarshal(in[0], &s) != nil {
			return reply(nil, errors.New("неверные настройки"))
		}
		return reply(nil, a.SaveSettings(s))
	case "NewDeviceID":
		return reply(nil, a.NewDeviceID())
	case "Connect":
		a.announce.Store(true)
		return reply(nil, a.Connect())
	case "Disconnect":
		a.Disconnect()
		return reply(nil, nil)
	case "Unlock":
		return reply(nil, nil)
	case "Revoked":
		a.Revoked()
		return reply(nil, nil)
	case "NetworkChanged":
		a.NetworkChanged()
		return reply(nil, nil)
	case "NetworkLost":
		a.NetworkLost()
		return reply(nil, nil)
	}
	return reply(nil, fmt.Errorf("неизвестный метод %s", method))
}

func reply(v any, err error) string {
	var b []byte
	if err != nil {
		b, _ = json.Marshal(map[string]any{"ok": false, "error": i18n.T(err.Error())})
	} else {
		b, _ = json.Marshal(map[string]any{"ok": true, "result": v})
	}
	return string(b)
}
