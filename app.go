package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"awgpilot/internal/autostart"
	"awgpilot/internal/flows"
	"awgpilot/internal/geoip"
	"awgpilot/internal/i18n"
	"awgpilot/internal/ipc"
	"awgpilot/internal/keyapi"
	"awgpilot/internal/netstat"
	"awgpilot/internal/netx"
	"awgpilot/internal/remotecfg"
	"awgpilot/internal/routing"
	"awgpilot/internal/speedtest"
	"awgpilot/internal/store"
	"awgpilot/internal/tray"
	"awgpilot/internal/tunnel"
	"awgpilot/internal/usage"
	"awgpilot/internal/vpnkey"
)

const appVersion = "0.9.1"

var dnsPresets = map[string][]string{
	"cloudflare": {"1.1.1.1", "1.0.0.1"},
	"quad9":      {"9.9.9.9", "149.112.112.112"},
	"adguard":    {"94.140.14.14", "94.140.15.15"},
}

// App is bound to the frontend; every exported method is callable from JS.
type App struct {
	ctx context.Context

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

	tun      svcTunnel  // the tunnel lives in the AWG Pilot service
	tunOp    sync.Mutex // serializes connect/disconnect so app rules always match the tunnel state
	tray     *tray.Tray
	quitting atomic.Bool
	wanted   atomic.Bool // the user's last choice: VPN on (Connect) or off (Disconnect)

	secretsBlocked bool        // secrets.bin is unreadable and could not be moved aside: never overwrite it
	autorun        atomic.Bool // cached: querying schtasks on every UI poll would spawn a process each time
	logs           logRing
	stop           chan struct{}

	meter        *netstat.Meter
	pingMs       atomic.Int64 // last round trip through the tunnel in ms; -1 = no reply
	announce     atomic.Bool  // toast the next successful handshake (tray/auto connects, recoveries)
	alert        atomic.Pointer[Alert]
	rec          recovery
	widgetMu     sync.Mutex
	widgetCmd    *exec.Cmd // running status window, if any
	usage        *usage.Store
	russiaStatus string // "N сетей, обновлён DD.MM.YYYY" after the list was loaded
	speed        speedtest.Tracker
}

// activateLocked derives per-profile state (decoded key, account data) from the active
// profile. Callers hold a.mu or run before any concurrency starts.
func (a *App) activateLocked() {
	a.key, a.account, a.countries, a.remote = nil, nil, nil, nil
	if p := a.secrets.Active(); p != nil && p.VPNKey != "" {
		a.key, _ = vpnkey.Decode(p.VPNKey)
	}
	a.alert.Store(nil)
}

// profileByID is the profile for id; it must be called with a.mu held.
func (a *App) profileByID(id string) *store.Profile { return a.secrets.Find(id) }

func NewApp() *App {
	a := &App{stop: make(chan struct{}), meter: netstat.NewMeter(120)}
	a.pingMs.Store(-1)
	return a
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	go noMaximize("AWG Pilot " + appVersion)
	st, err := store.Open()
	if err != nil {
		a.logf("не удалось открыть папку настроек: %v", err)
		return
	}
	a.st = st
	a.settings, err = st.LoadSettings()
	i18n.SetLanguage(i18n.Resolve(a.settings.Language, i18n.System()))
	if err != nil {
		a.logf("настройки повреждены, использую значения по умолчанию: %v", err)
	}
	a.secrets, err = st.LoadSecrets()
	if err != nil {
		a.secrets = store.Secrets{}
		name, qerr := st.QuarantineSecrets()
		if qerr != nil && !errors.Is(qerr, os.ErrNotExist) {
			a.secretsBlocked = true // never write over keys that only failed to decrypt
			a.logf("не удалось расшифровать сохранённые ключи и конфиги (%v), а отложить файл не вышло (%v): изменения профилей сохраняться не будут", err, qerr)
		} else {
			a.logf("не удалось расшифровать сохранённые ключи и конфиги (%v); файл сохранён как %s", err, name)
		}
	} else {
		a.saveSecrets() // persists the migration from the single-profile format
	}
	a.activateLocked()
	a.usage = usage.Open(filepath.Join(st.Dir(), "usage.json"))
	a.ensureDeviceID()
	a.autorun.Store(autostart.Enabled())
	if a.autorun.Load() {
		// Re-point the task at this exe: after installing to Program Files the old task would
		// still start the copy the user enabled autostart from.
		if exe, err := os.Executable(); err == nil {
			if err := autostart.Enable(exe, "--tray"); err != nil {
				a.logf("не удалось обновить задачу автозапуска: %v", err)
			}
		}
	}
	a.applyAppRules() // programs restricted to the VPN stay offline until it is up

	a.tray = tray.Start(tray.Callbacks{
		Show: a.ShowWindow,
		Toggle: func() {
			if a.tun.IsUp() {
				a.Disconnect()
				return
			}
			a.announce.Store(true)
			if err := a.Connect(); err != nil {
				a.logf("подключение из трея не удалось: %v", err)
				a.notify("Не удалось подключиться", err.Error(), "")
				a.ShowWindow()
			}
		},
		Quit:          a.Quit,
		SelectProfile: a.selectProfileByIndex,
		ToggleWidget: func() {
			a.mu.Lock()
			on := !a.settings.ShowWidget
			a.mu.Unlock()
			a.SetWidget(on)
		},
	})
	a.trayProfiles()
	if a.settings.ShowWidget {
		a.startWidget()
	}
	a.initNotifications()
	go a.monitor()
	go a.watchdog()
	go a.subscriptionWatcher()
	if p := a.secrets.Active(); a.settings.AutoConnect && p != nil && p.RawConfig != "" {
		go func() {
			a.announce.Store(true)
			if err := a.Connect(); err != nil {
				a.logf("автоподключение не удалось: %v", err)
				a.notify("Автоподключение не удалось", err.Error(), "")
			}
		}()
	}
}

func (a *App) shutdown(context.Context) {
	close(a.stop)
	a.tunOp.Lock()
	a.tun.Down(true) // quitting is a deliberate disconnect
	a.tunOp.Unlock()
	if a.usage != nil {
		a.usage.Save()
	}
	a.stopWidget()
	if a.tray != nil {
		a.tray.Close()
	}
}

// beforeClose hides the window to the tray instead of quitting, unless the user chose Exit.
func (a *App) beforeClose(ctx context.Context) bool {
	if a.quitting.Load() {
		return false
	}
	a.mu.Lock()
	toTray := a.settings.CloseToTray
	a.mu.Unlock()
	if !toTray {
		return false
	}
	wruntime.WindowHide(ctx)
	return true
}

func (a *App) ShowWindow() {
	if a.ctx == nil {
		return
	}
	wruntime.WindowShow(a.ctx)
	wruntime.WindowUnminimise(a.ctx)
	go noMaximize("AWG Pilot " + appVersion) // a window started hidden in the tray gets it here
}

// Quit disconnects and exits the program.
func (a *App) Quit() {
	a.quitting.Store(true)
	wruntime.Quit(a.ctx)
}

// ensureDeviceID gives AWG Pilot its own device ID (sent as X-Device-Id) on first start.
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

// NewDeviceID creates a fresh device ID; the provider may count it as another device.
func (a *App) NewDeviceID() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.settings.DeviceID, a.deviceSrc = newDeviceID(), "generated"
	a.saveSettings()
	a.logf("создан новый ID устройства")
	return nil
}

func (a *App) saveSettings() {
	if a.st != nil {
		if err := a.st.SaveSettings(a.settings); err != nil {
			a.logf("не удалось сохранить настройки: %v", err)
		}
	}
}

func (a *App) saveSecrets() error {
	if a.secretsBlocked {
		return errors.New("сохранённые ключи не читаются, поэтому изменения профилей не сохраняются — перезапустите программу")
	}
	if a.st == nil {
		return errors.New("хранилище настроек недоступно")
	}
	if err := a.st.SaveSecrets(a.secrets); err != nil {
		return fmt.Errorf("не удалось сохранить ключ и конфиг: %w", err)
	}
	return nil
}

func (a *App) userAgent() string {
	if a.settings.UserAgent != "" {
		return a.settings.UserAgent
	}
	return "AWGPilot/" + appVersion + " (Windows)"
}

func (a *App) client() (*keyapi.Client, error) {
	if a.key == nil {
		return nil, errors.New("сначала добавьте ключ vpn:// на вкладке «Профили и ключи»")
	}
	if a.settings.DeviceID == "" {
		return nil, errors.New("выберите ID устройства на вкладке «Профили и ключи»")
	}
	base := a.key.APIEndpoint
	if a.remote != nil && a.remote.APIBaseURL != "" && strings.HasPrefix(a.remote.APIBaseURL, "https://") {
		base = a.remote.APIBaseURL
	}
	return keyapi.New(base, a.settings.DeviceID, a.userAgent()), nil
}

func (a *App) reqCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// ---- State exposed to the UI -------------------------------------------------------------

type State struct {
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
	Tunnel       tunnel.Stats           `json:"tunnel"`
	Warnings     []string               `json:"warnings"`
	ServiceOK    bool                   `json:"service_ok"` // the AWG Pilot service answers
	Autostart    bool                   `json:"autostart"`
	Version      string                 `json:"version"`
	Speed        SpeedInfo              `json:"speed"`
	Alert        *Alert                 `json:"alert"`
	Usage        usage.Summary          `json:"usage"`
	RussiaList   string                 `json:"russia_list"` // status of the Russian networks list
	SpeedTest    speedtest.State        `json:"speedtest"`
	Lang         string                 `json:"lang"`      // interface language in effect: "ru" or "en"
	OtherVPN     string                 `json:"other_vpn"` // adapter of another VPN that carries the traffic instead
}

// ProfileInfo is a secret-free description of a profile.
type ProfileInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	HasKey    bool   `json:"has_key"`
	MaskedKey string `json:"masked_key"`
	Endpoint  string `json:"endpoint"`
	HasConfig bool   `json:"has_config"`
	ConfigFor string `json:"config_for"`
}

// SpeedInfo is the live throughput and latency through the tunnel.
type SpeedInfo struct {
	RxBps   float64         `json:"rx_bps"`
	TxBps   float64         `json:"tx_bps"`
	PingMs  int64           `json:"ping_ms"` // -1 = no reply / not measured
	History []netstat.Point `json:"history"`
}

// Alert is a persistent problem banner with an optional action.
type Alert struct {
	Kind    string `json:"kind"` // "server_down", "expiring"
	Message string `json:"message"`
	Action  string `json:"action"` // "change_server", "" = none
}

// ConfigInfo is a secret-free summary of the stored AWG config.
type ConfigInfo struct {
	Endpoint string   `json:"endpoint"`
	Address  []string `json:"address"`
	DNS      []string `json:"dns"`
	MTU      int      `json:"mtu"`
	AWG      string   `json:"awg"`
}

func profileInfo(p store.Profile) ProfileInfo {
	pi := ProfileInfo{ID: p.ID, Name: p.Name, HasConfig: p.RawConfig != "", ConfigFor: p.ConfigFor}
	if k, err := vpnkey.Decode(p.VPNKey); err == nil {
		pi.HasKey, pi.MaskedKey, pi.Endpoint = true, k.Masked(), k.APIEndpoint
	}
	return pi
}

func (a *App) GetState() State {
	autostartOn := a.autorun.Load()
	a.mu.Lock()
	defer a.mu.Unlock()
	s := State{
		HasKey:       a.key != nil,
		ActiveID:     a.secrets.ActiveID,
		Settings:     a.settings,
		DeviceSource: a.deviceSrc,
		Account:      a.account,
		Countries:    a.countries,
		Tunnel:       a.tun.Stats(),
		Warnings:     a.warnings,
		ServiceOK:    a.tun.Available(),
		Autostart:    autostartOn,
		Version:      appVersion,
		RussiaList:   a.russiaStatus,
	}
	for _, p := range a.secrets.Profiles {
		s.Profiles = append(s.Profiles, profileInfo(p))
	}
	if a.usage != nil {
		s.Usage = a.usage.Summary(time.Now())
	}
	s.Settings.DeviceID = "" // not needed in the UI
	last, hist := a.meter.Snapshot()
	s.Speed = SpeedInfo{RxBps: last.Rx, TxBps: last.Tx, PingMs: a.pingMs.Load(), History: hist}
	if !s.Tunnel.Up {
		s.Speed = SpeedInfo{PingMs: -1}
	}
	if al := a.alert.Load(); al != nil {
		c := *al
		c.Message = i18n.T(c.Message)
		s.Alert = &c
	}
	s.Lang = i18n.Lang()
	s.RussiaList = i18n.T(s.RussiaList)
	s.ConfigFor = i18n.T(s.ConfigFor)
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
	if s.Tunnel.Up && a.settings.Split.Mode != routing.ModeInclude {
		s.OtherVPN = otherVPN()
	}
	s.SpeedTest = a.speed.Snapshot()
	s.SpeedTest.Error = i18n.T(s.SpeedTest.Error)
	if a.key != nil {
		s.MaskedKey, s.APIEndpoint = a.key.Masked(), a.key.APIEndpoint
	}
	if a.remote != nil {
		s.Connections = a.remote.Connections
	}
	if p := a.secrets.Active(); p != nil && p.RawConfig != "" {
		s.HasConfig, s.ConfigFor = true, p.ConfigFor
		if c, err := tunnel.ParseConfig(p.RawConfig); err == nil {
			ci := &ConfigInfo{MTU: int(c.Interface.MTU), Endpoint: c.Peers[0].Endpoint.String()}
			for _, ad := range c.Interface.Addresses {
				ci.Address = append(ci.Address, ad.String())
			}
			for _, d := range c.Interface.DNS {
				ci.DNS = append(ci.DNS, d.String())
			}
			ci.AWG = fmt.Sprintf("Jc=%d Jmin=%d Jmax=%d S1..S4=%d/%d/%d/%d", c.Interface.JunkPacketCount, c.Interface.JunkPacketMinSize, c.Interface.JunkPacketMaxSize,
				c.Interface.InitPacketJunkSize, c.Interface.ResponsePacketJunkSize, c.Interface.CookieReplyPacketJunkSize, c.Interface.TransportPacketJunkSize)
			s.ConfigInfo = ci
		}
	}
	return s
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

// Translate shows a message from the program (an error, for example) in the interface language.
func (a *App) Translate(s string) string { return i18n.T(s) }

// SetLanguage switches the interface language: "" follows the system, "ru" or "en".
func (a *App) SetLanguage(lang string) error {
	a.mu.Lock()
	a.settings.Language = lang
	a.saveSettings()
	widget := a.settings.ShowWidget
	a.mu.Unlock()
	i18n.SetLanguage(i18n.Resolve(lang, i18n.System()))
	if a.tray != nil {
		a.tray.Retitle()
	}
	if widget { // the status window takes the language when it starts
		a.stopWidget()
		return a.startWidget()
	}
	return nil
}

// SpeedTest measures download and upload against Cloudflare through the VPN adapter.
func (a *App) SpeedTest() error {
	if !a.tun.IsUp() {
		return errors.New("сначала подключите VPN")
	}
	a.mu.Lock()
	split := a.settings.Split
	a.mu.Unlock()
	if split.Mode == routing.ModeInclude && !strings.Contains(strings.Join(split.Entries, " "), "cloudflare") {
		return errors.New("в режиме «только список» сервер теста идёт мимо VPN — добавьте speed.cloudflare.com в список")
	}
	dial, err := tunnelDialer()
	if err != nil {
		return err
	}
	if !a.speed.Start(dial, func(r speedtest.Result, err error) {
		if err != nil {
			a.logf("тест скорости не удался: %v", err)
			return
		}
		a.logf("тест скорости через VPN (IP %s, %s): загрузка %.1f Мбит/с, отдача %.1f Мбит/с", r.IP, r.Loc, r.DownMbps, r.UpMbps)
	}) {
		return errors.New("тест скорости уже идёт")
	}
	a.logf("тест скорости запущен")
	return nil
}

// ResetUsage clears the traffic statistics.
func (a *App) ResetUsage() error {
	if a.usage == nil {
		return nil
	}
	a.logf("статистика трафика очищена")
	return a.usage.Reset()
}

// TrafficLog returns which programs sent traffic through the VPN and where on one day
// ("" = today). The service records it while the log is switched on.
func (a *App) TrafficLog(day string) (flows.Report, error) {
	rep, err := a.tun.c.Flows(day)
	for i := range rep.Rows {
		rep.Rows[i].App, rep.Rows[i].Host = i18n.T(rep.Rows[i].App), i18n.T(rep.Rows[i].Host)
	}
	return rep, err
}

// SetTrafficLog switches the per-program traffic log on or off.
func (a *App) SetTrafficLog(on bool) error { return a.tun.c.SetFlowLog(on) }

// ClearTrafficLog deletes the per-program traffic log.
func (a *App) ClearTrafficLog() error {
	a.logf("журнал трафика по программам очищен")
	return a.tun.c.ClearFlows()
}

// ---- Profiles -----------------------------------------------------------------------------

// addProfileLocked appends a profile and makes it active.
func (a *App) addProfileLocked(p store.Profile) {
	p.ID = store.NewID()
	if strings.TrimSpace(p.Name) == "" {
		p.Name = fmt.Sprintf("Профиль %d", len(a.secrets.Profiles)+1)
	}
	a.secrets.Profiles = append(a.secrets.Profiles, p)
	a.secrets.ActiveID = p.ID
	a.activateLocked()
}

// switchProfile runs change with the tunnel down when the active profile changes, and brings
// the tunnel back up on the new profile if it was running.
func (a *App) switchProfile(change func() error) error {
	a.tunOp.Lock()
	defer a.tunOp.Unlock()
	a.mu.Lock()
	before := a.secrets.ActiveID
	a.mu.Unlock()
	wasUp := a.tun.IsUp()
	if wasUp {
		a.disconnectLocked(false) // keep an armed lockdown while switching
	}
	if err := change(); err != nil {
		if wasUp {
			a.connectLocked()
		}
		return err
	}
	a.mu.Lock()
	switched := a.secrets.ActiveID != before
	p := a.secrets.Active()
	canConnect := p != nil && p.RawConfig != ""
	a.mu.Unlock()
	if switched {
		a.trayProfiles()
	}
	if wasUp && canConnect {
		return a.connectLocked()
	}
	if wasUp {
		a.tun.Unlock() // nothing to reconnect to on the new profile
	}
	return nil
}

// AddKey adds a vpn:// key as a new profile (or activates the profile that already has it).
// It makes no network request.
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
		return a.importConfig(n.Config, n.Name, "ключ AmneziaVPN "+n.Host)
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

// importConfig stores a wg-quick/AmneziaWG config as a new active profile.
func (a *App) importConfig(text, name, origin string) error {
	if _, err := tunnel.ParseConfig(text); err != nil {
		return fmt.Errorf("это не конфиг AmneziaWG/WireGuard: %w", err)
	}
	return a.switchProfile(func() error {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.addProfileLocked(store.Profile{Name: name, RawConfig: text, ConfigFor: origin})
		a.logf("конфиг импортирован как профиль «%s»", a.secrets.Active().Name)
		return a.saveSecrets()
	})
}

// ImportConfig stores pasted config text as a new profile.
func (a *App) ImportConfig(text string) error {
	return a.importConfig(text, "", "импортирован")
}

// ImportConfigFile lets the user pick a .conf file; it becomes a new profile named after the file.
func (a *App) ImportConfigFile() error {
	p, err := wruntime.OpenFileDialog(a.ctx, wruntime.OpenDialogOptions{
		Title:   i18n.T("Импорт конфигурации AmneziaWG"),
		Filters: []wruntime.FileFilter{{DisplayName: i18n.T("Конфиг или ключ (*.conf, *.vpn)"), Pattern: "*.conf;*.vpn"}},
	})
	if err != nil || p == "" {
		return err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return fmt.Errorf("не удалось прочитать файл: %w", err)
	}
	if text := strings.TrimSpace(string(b)); strings.HasPrefix(strings.ToLower(text), "vpn://") {
		return a.AddKey(text) // a key saved from AmneziaVPN or the provider
	}
	name := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
	return a.importConfig(string(b), name, "файл "+filepath.Base(p))
}

// ExportConfig returns the active profile's config text.
func (a *App) ExportConfig() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.secrets.Active()
	if p == nil || p.RawConfig == "" {
		return "", errors.New("у активного профиля ещё нет конфига")
	}
	return p.RawConfig, nil
}

// ExportConfigFile saves the active config to a user-chosen .conf file.
func (a *App) ExportConfigFile() error {
	text, err := a.ExportConfig()
	if err != nil {
		return err
	}
	a.mu.Lock()
	name := "awgpilot"
	if p := a.secrets.Active(); p != nil {
		name = p.Name
	}
	a.mu.Unlock()
	p, err := wruntime.SaveFileDialog(a.ctx, wruntime.SaveDialogOptions{
		Title:           i18n.T("Экспорт конфигурации"),
		DefaultFilename: name + ".conf",
		Filters:         []wruntime.FileFilter{{DisplayName: "AmneziaWG (*.conf)", Pattern: "*.conf"}},
	})
	if err != nil || p == "" {
		return err
	}
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		return fmt.Errorf("не удалось записать файл: %w", err)
	}
	return nil
}

// SelectProfile makes another profile active, reconnecting on it if the VPN was on.
func (a *App) SelectProfile(id string) error {
	return a.switchProfile(func() error {
		a.mu.Lock()
		defer a.mu.Unlock()
		p := a.profileByID(id)
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
	p := a.profileByID(id)
	if p == nil {
		return errors.New("профиль не найден")
	}
	p.Name = name
	defer a.trayProfilesLocked()
	return a.saveSecrets()
}

// DeleteProfile removes a profile with its key and config.
func (a *App) DeleteProfile(id string) error {
	return a.switchProfile(func() error {
		a.mu.Lock()
		defer a.mu.Unlock()
		p := a.profileByID(id)
		if p == nil {
			return errors.New("профиль не найден")
		}
		name := p.Name
		wasActive := a.secrets.ActiveID == id
		a.secrets.Remove(id)
		if wasActive {
			a.activateLocked()
		}
		a.logf("профиль «%s» удалён", name)
		a.trayProfilesLocked()
		return a.saveSecrets()
	})
}

// trayProfiles mirrors the profile list in the tray menu.
func (a *App) trayProfiles() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.trayProfilesLocked()
}

func (a *App) trayProfilesLocked() {
	if a.tray == nil {
		return
	}
	var names []string
	active := -1
	for i, p := range a.secrets.Profiles {
		names = append(names, p.Name)
		if p.ID == a.secrets.ActiveID {
			active = i
		}
	}
	a.tray.SetProfiles(names, active)
}

// selectProfileByIndex handles a click in the tray's profile submenu.
func (a *App) selectProfileByIndex(i int) {
	a.mu.Lock()
	var id string
	if i >= 0 && i < len(a.secrets.Profiles) {
		id = a.secrets.Profiles[i].ID
	}
	a.mu.Unlock()
	if id == "" {
		return
	}
	if err := a.SelectProfile(id); err != nil {
		a.logf("переключение профиля не удалось: %v", err)
		a.notify("Профиль не переключён", err.Error(), "")
	}
}

// ---- Provider API (each call is user-initiated) ---------------------------------------------

// apiSnapshot captures what a network call needs so a.mu is not held during I/O.
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
	// The public desktop config describes the default gateway only; other keys use their own endpoint.
	needRemote := a.remote == nil && a.key != nil && strings.HasPrefix(a.key.APIEndpoint, remotecfg.GatewayAPI)
	pid := a.secrets.ActiveID // answers are kept only for the profile they were asked for
	a.mu.Unlock()
	if needRemote {
		ctx, cancel := a.reqCtx()
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
	ctx, cancel := a.reqCtx()
	defer cancel()
	req, err := snap.c.GetRequest(ctx, snap.apiKey)
	if err != nil {
		a.logf("запрос статуса подписки не удался: %v", err)
		return nil, err
	}
	countries, cerr := snap.c.GetCountries(ctx)
	if cerr != nil {
		a.logf("список стран не загрузился: %v", cerr)
	} else if len(countries) == 0 {
		a.logf("провайдер не предлагает выбор страны — используется сервер, который он назначает сам")
	}
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

// DownloadConfig asks the gateway for a config for the selected country/connection type.
// forceRebind must only be true after the user confirmed moving the key to this PC.
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
	ctx, cancel := a.reqCtx()
	defer cancel()
	raw, err := snap.c.DownloadAWGKey(ctx, snap.apiKey, opts)
	if err != nil {
		a.logf("получение конфига не удалось: %v", err)
		return err
	}
	if _, err := tunnel.ParseConfig(raw); err != nil {
		return fmt.Errorf("сервер прислал непригодный конфиг: %w", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// Store into the profile the request was made for, even if the user switched meanwhile.
	p := a.profileByID(snap.profileID)
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

func (a *App) CheckChangeServer() (bool, error) {
	snap, err := a.snapshot()
	if err != nil {
		return false, err
	}
	return a.checkChangeFor(snap)
}

func (a *App) checkChangeFor(snap apiSnapshot) (bool, error) {
	ctx, cancel := a.reqCtx()
	defer cancel()
	return snap.c.CheckChangeServer(ctx, snap.apiKey, snap.settings.KeyType, snap.settings.Country)
}

// ChangeServer moves the key to another server, then downloads the new config.
func (a *App) ChangeServer() error {
	snap, err := a.snapshot()
	if err != nil {
		return err
	}
	return a.changeServerFor(snap)
}

// changeServerFor changes the server and downloads the new config for the same profile.
func (a *App) changeServerFor(snap apiSnapshot) error {
	ctx, cancel := a.reqCtx()
	err := snap.c.ChangeServer(ctx, snap.apiKey, snap.settings.KeyType, snap.settings.Country)
	cancel()
	if err != nil {
		a.logf("смена сервера не удалась: %v", err)
		return err
	}
	a.logf("сервер сменён")
	return a.downloadFor(snap, false)
}

// ---- Settings ----------------------------------------------------------------------------

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
	for _, p := range s.VPNOnlyApps {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("программа не найдена: %s", p)
		}
	}
	a.mu.Lock()
	appsChanged := strings.Join(a.settings.VPNOnlyApps, "\n") != strings.Join(s.VPNOnlyApps, "\n")
	s.DeviceID = a.settings.DeviceID // never changed from the UI
	a.settings = s
	a.saveSettings()
	a.mu.Unlock()
	if appsChanged {
		a.tunOp.Lock()
		err := a.applyAppRules()
		a.tunOp.Unlock()
		return err
	}
	return nil
}

// PickApp opens a file dialog to choose a program for the "only through VPN" list.
func (a *App) PickApp() (string, error) {
	return wruntime.OpenFileDialog(a.ctx, wruntime.OpenDialogOptions{
		Title:   i18n.T("Программа, которой разрешён интернет только через VPN"),
		Filters: []wruntime.FileFilter{{DisplayName: i18n.T("Программы (*.exe)"), Pattern: "*.exe"}},
	})
}

// SetAutostart enables or disables starting with Windows (minimized to the tray).
func (a *App) SetAutostart(on bool) error {
	defer func() { a.autorun.Store(autostart.Enabled()) }()
	if !on {
		if err := autostart.Disable(); err != nil {
			return err
		}
		a.logf("автозапуск с Windows выключен")
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := autostart.Enable(exe, "--tray"); err != nil {
		return err
	}
	a.logf("автозапуск с Windows включён (программа стартует свёрнутой в трей)")
	return nil
}

// applyAppRules installs the per-app rules for the current tunnel state. Callers hold tunOp
// (or run before any tunnel can exist).
func (a *App) applyAppRules() error {
	a.mu.Lock()
	apps := append([]string(nil), a.settings.VPNOnlyApps...)
	a.mu.Unlock()
	if err := a.tun.c.SetApps(apps); err != nil {
		if errors.Is(err, ipc.ErrNoService) && len(apps) == 0 {
			return nil
		}
		a.logf("правила для программ не применены: %v", err)
		return err
	}
	return nil
}

// ---- Status window ------------------------------------------------------------------------

// SetWidget shows or hides the always-on-top status window and remembers the choice.
// SetSkin stores the chosen colour theme (the page applies it itself).
func (a *App) SetSkin(skin string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.settings.Skin = skin
	a.saveSettings()
	return nil
}

func (a *App) SetWidget(on bool) error {
	a.mu.Lock()
	a.settings.ShowWidget = on
	a.saveSettings()
	a.mu.Unlock()
	if a.tray != nil {
		a.tray.SetWidget(on)
	}
	if on {
		return a.startWidget()
	}
	a.stopWidget()
	return nil
}

func (a *App) startWidget() error {
	a.widgetMu.Lock()
	defer a.widgetMu.Unlock()
	if a.widgetCmd != nil {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "--widget", "--lang="+i18n.Lang())
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("окошко статуса не запустилось: %w", err)
	}
	a.widgetCmd = cmd
	if a.tray != nil {
		a.tray.SetWidget(true)
	}
	go func() {
		cmd.Wait()
		a.widgetMu.Lock()
		closedByUser := a.widgetCmd == cmd // stopWidget clears the field before killing
		if closedByUser {
			a.widgetCmd = nil
		}
		a.widgetMu.Unlock()
		if closedByUser && !a.quitting.Load() {
			a.mu.Lock()
			a.settings.ShowWidget = false
			a.saveSettings()
			a.mu.Unlock()
			if a.tray != nil {
				a.tray.SetWidget(false)
			}
		}
	}()
	return nil
}

func (a *App) stopWidget() {
	a.widgetMu.Lock()
	cmd := a.widgetCmd
	a.widgetCmd = nil
	a.widgetMu.Unlock()
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Kill()
	}
}

// ---- Tunnel -------------------------------------------------------------------------------

// dnsServers returns the DNS override for s, or nil to keep the provider's DNS.
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

func (a *App) Connect() error {
	a.wanted.Store(true)
	a.tunOp.Lock()
	defer a.tunOp.Unlock()
	return a.connectLocked()
}

func (a *App) connectLocked() error {
	a.mu.Lock()
	var raw, profName string
	if p := a.secrets.Active(); p != nil {
		raw, profName = p.RawConfig, p.Name
	}
	settings := a.settings
	a.mu.Unlock()
	if raw == "" {
		return errors.New("у профиля нет конфига: нажмите «Обновить конфиг» или импортируйте .conf")
	}
	cfg, err := tunnel.ParseConfig(raw)
	if err != nil {
		return fmt.Errorf("сохранённый конфиг повреждён: %w", err)
	}
	dns := dnsServers(settings)
	tunnelDNS := dns
	if len(tunnelDNS) == 0 {
		for _, d := range cfg.Interface.DNS {
			if ad, ok := netip.AddrFromSlice(d); ok {
				tunnelDNS = append(tunnelDNS, ad.Unmap())
			}
		}
	}
	var bypass []netip.Prefix
	if settings.Split.BypassRussia && settings.Split.Mode != routing.ModeInclude {
		ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
		list, err := geoip.Get(ctx, filepath.Join(a.st.Dir(), "ru-ipv4.txt"))
		cancel()
		switch {
		case list == nil:
			return fmt.Errorf("не удалось получить список российских сетей: %w", err)
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
		return err
	}
	if len(bypass) > 0 {
		a.logf("Россия мимо VPN: %d российских сетей напрямую, маршрутов через VPN: %d", len(bypass), len(plan.AllowedIPs))
	}
	a.logf("подключаюсь: профиль «%s»", profName)
	a.mu.Lock()
	a.warnings = plan.Warnings
	a.mu.Unlock()
	for _, w := range plan.Warnings {
		a.logf("раздельный туннель: %s", w)
	}
	if settings.KillSwitch && !plan.FullTunnel {
		a.logf("kill switch не действует, пока включён раздельный туннель или «локальная сеть мимо VPN»")
	}
	err = a.tun.Up(tunnel.Options{
		RawConfig:  raw,
		AllowedIPs: plan.AllowedIPs,
		FullTunnel: plan.FullTunnel,
		KillSwitch: settings.KillSwitch,
		BlockIPv6:  settings.BlockIPv6,
		DNS:        dns,
		MTU:        settings.MTU,
		Lockdown:   settings.LockdownOnDrop,
		Logf:       a.logf,
	})
	if err != nil {
		a.logf("подключение не удалось: %v", err)
		return err
	}
	a.meter.Reset()
	a.pingMs.Store(-1)
	return nil
}

// Disconnect is the user's deliberate disconnect: it also reopens a blocked internet.
func (a *App) Disconnect() {
	a.wanted.Store(false)
	a.tunOp.Lock()
	defer a.tunOp.Unlock()
	a.disconnectLocked(true)
}

// disconnectLocked stops the tunnel. release=false (reconnects, failover) keeps an armed
// lockdown so nothing leaks while the VPN is being restored.
func (a *App) disconnectLocked(release bool) {
	if !a.tun.IsUp() {
		if release {
			a.tun.Down(true) // lift a lockdown left by a drop
		}
		return
	}
	a.tun.Down(release)
	a.logf("отключено")
}

// Unlock reopens the internet after a VPN drop without connecting.
func (a *App) Unlock() error {
	a.tunOp.Lock()
	defer a.tunOp.Unlock()
	if err := a.tun.Unlock(); err != nil {
		return err
	}
	a.logf("интернет разблокирован вручную")
	return nil
}

// ---- log ring -------------------------------------------------------------------------------

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
	a.logs.add(time.Now().Format("15:04:05 ") + fmt.Sprintf(format, args...))
}
