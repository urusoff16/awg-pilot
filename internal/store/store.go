// Package store persists settings in %APPDATA%\AWGPilot. Secrets (the access key and the
// issued AWG config, which contains a private key) are encrypted with Windows DPAPI.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"awgpilot/internal/routing"
)

// Settings are non-secret user preferences.
type Settings struct {
	Country     string           `json:"country"`      // ISO code; "" = server default
	KeyType     string           `json:"key_type"`     // connection type from the remote config
	KillSwitch  bool             `json:"kill_switch"`  // block traffic outside the tunnel (full tunnel only)
	DNSPreset   string           `json:"dns_preset"`   // "provider", "cloudflare", "quad9", "adguard", "custom"
	CustomDNS   []string         `json:"custom_dns"`   // used with DNSPreset "custom"
	MTU         int              `json:"mtu"`          // 0 = from config
	AutoConnect bool             `json:"auto_connect"` // connect on start
	Reconnect   bool             `json:"reconnect"`    // restart the tunnel when the handshake goes stale
	Split       routing.Settings `json:"split"`
	BlockIPv6   bool             `json:"block_ipv6"`    // blackhole IPv6 while connected (prevents IPv6 leaks)
	VPNOnlyApps []string         `json:"vpn_only_apps"` // programs allowed to reach the network only through the tunnel
	CloseToTray bool             `json:"close_to_tray"` // closing the window keeps the app running in the tray
	// Notifications shows Windows toasts for connection loss, recovery and subscription expiry.
	Notifications bool `json:"notifications"`
	// CheckSubscription polls the subscription status every few hours to warn before it expires.
	CheckSubscription bool `json:"check_subscription"`
	// AutoChangeServer requests another server when the current one stays unreachable after a
	// reconnect and a config refresh. Off by default: the provider allows one change per day.
	AutoChangeServer bool `json:"auto_change_server"`
	// LockdownOnDrop keeps the internet closed when the VPN drops unexpectedly, until it is
	// restored (other profiles are tried too) or the user disconnects.
	LockdownOnDrop bool `json:"lockdown_on_drop"`
	// ShowWidget shows the small always-on-top status window.
	ShowWidget bool `json:"show_widget"`
	// Skin is the colour theme of the window: "" (dark), mint, ocean, sunset or neon.
	Skin string `json:"skin"`
	// AppMode and AppPackages are Android's per-app split: "all" (every app through the VPN),
	// "include" (only AppPackages) or "exclude" (all except AppPackages).
	AppMode     string   `json:"app_mode"`
	AppPackages []string `json:"app_packages"`
	DeviceID    string   `json:"device_id"`  // X-Device-Id; AWG Pilot's own random ID
	UserAgent   string   `json:"user_agent"` // optional override
}

func DefaultSettings() Settings {
	return Settings{DNSPreset: "provider", Reconnect: true, BlockIPv6: true, CloseToTray: true, Notifications: true, CheckSubscription: true,
		Split: routing.Settings{Mode: routing.ModeFull}}
}

// Profile is one subscription key and/or AmneziaWG config.
type Profile struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	VPNKey    string `json:"vpn_key,omitempty"`
	RawConfig string `json:"raw_config,omitempty"`
	ConfigFor string `json:"config_for,omitempty"` // where the config came from
}

// Secrets never leave this machine unencrypted.
type Secrets struct {
	Profiles []Profile `json:"profiles"`
	ActiveID string    `json:"active_id"`

	// Single-profile fields written by versions before 0.4; migrated by Normalize.
	VPNKey    string `json:"vpn_key,omitempty"`
	RawConfig string `json:"raw_config,omitempty"`
	ConfigFor string `json:"config_for,omitempty"`
}

// Normalize migrates the legacy single profile and repairs a dangling ActiveID.
func (s *Secrets) Normalize() {
	if s.VPNKey != "" || s.RawConfig != "" {
		s.Profiles = append([]Profile{{ID: NewID(), Name: "Профиль 1", VPNKey: s.VPNKey, RawConfig: s.RawConfig, ConfigFor: s.ConfigFor}}, s.Profiles...)
		s.VPNKey, s.RawConfig, s.ConfigFor = "", "", ""
	}
	if s.Active() == nil && len(s.Profiles) > 0 {
		s.ActiveID = s.Profiles[0].ID
	}
}

// Active returns the active profile, or nil when there is none.
func (s *Secrets) Active() *Profile { return s.Find(s.ActiveID) }

func (s *Secrets) Find(id string) *Profile {
	for i := range s.Profiles {
		if s.Profiles[i].ID == id {
			return &s.Profiles[i]
		}
	}
	return nil
}

// Remove deletes a profile; the first remaining one becomes active if needed.
func (s *Secrets) Remove(id string) {
	out := s.Profiles[:0]
	for _, p := range s.Profiles {
		if p.ID != id {
			out = append(out, p)
		}
	}
	s.Profiles = out
	if s.ActiveID == id {
		s.ActiveID = ""
	}
	s.Normalize()
}

// NewID returns a short random profile identifier.
func NewID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// SecretKey encrypts secrets.bin on platforms without DPAPI (unused on Windows). On Android it is
// a 32-byte key kept wrapped by the Android Keystore and handed over by the app at startup.
var SecretKey []byte

// RequireKey refuses to write secrets without SecretKey instead of storing them in the clear
// (Android sets it: a Keystore failure must not turn into plaintext keys on disk).
var RequireKey bool

type Store struct {
	dir string
	mu  sync.Mutex
}

func Open() (*Store, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(base, "AWGPilot")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// OpenDir uses dir instead of the per-user config folder (Android passes its private files dir).
func OpenDir(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) Dir() string { return s.dir }

func (s *Store) LoadSettings() (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := DefaultSettings()
	b, err := os.ReadFile(filepath.Join(s.dir, "settings.json"))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(b, &st)
	return st, err
}

func (s *Store) SaveSettings(st Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.dir, "settings.json"), b)
}

func (s *Store) LoadSecrets() (Secrets, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var sec Secrets
	b, err := os.ReadFile(filepath.Join(s.dir, "secrets.bin"))
	if errors.Is(err, os.ErrNotExist) {
		return sec, nil
	}
	if err != nil {
		return sec, err
	}
	plain, err := unprotect(b)
	if err != nil {
		return sec, err
	}
	err = json.Unmarshal(plain, &sec)
	sec.Normalize()
	return sec, err
}

func (s *Store) SaveSecrets(sec Secrets) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	plain, err := json.Marshal(sec)
	if err != nil {
		return err
	}
	b, err := protect(plain)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.dir, "secrets.bin"), b)
}

// QuarantineSecrets moves an unreadable secrets.bin aside, so saving new profiles can never
// overwrite keys that only failed to decrypt here (another Windows account, a lost Keystore key).
// It returns the new file name.
func (s *Store) QuarantineSecrets() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := filepath.Join(s.dir, "secrets.bin")
	if _, err := os.Stat(src); err != nil {
		return "", err
	}
	dst := filepath.Join(s.dir, "secrets.unreadable-"+time.Now().Format("20060102-150405")+".bin")
	return filepath.Base(dst), os.Rename(src, dst)
}

func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
