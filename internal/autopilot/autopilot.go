// Package autopilot chooses the profile by the kind of network and keeps the server pool fresh.
// Windows and Android share it; each provides a Host.
//
// The rules (when Settings.AutoProfile is on):
//   - every kind of network has its profile (Settings.NetProfiles); when the network changes, the
//     VPN moves to the profile for the new one;
//   - while that profile is weak (delay above Settings.WeakPingMs or half the checks lost for a
//     minute and more) the fallback profile carries the traffic; a dead tunnel is the watchdog's
//     business, and its failover also prefers the fallback (NextProfile);
//   - while the fallback is in use, the profile for the network is checked in the background every
//     Settings.CheckEveryMin minutes and taken back once it answers well;
//   - a profile the user picked by hand stays until the network changes.
//
// Independently of AutoProfile, subscription servers are checked every CheckEveryMin minutes while
// the VPN is on, so the pool used for switching is sorted by fresh delays; a weak subscription
// server is replaced by a clearly faster one.
package autopilot

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"awgpilot/internal/store"
	"awgpilot/internal/subscription"
)

// Tick is how often the controller looks at the tunnel.
const Tick = 5 * time.Second

const (
	window      = 2 * time.Minute  // ping samples the weak verdict looks at
	minSamples  = 8                // fewer samples say nothing yet
	weakFor     = 60 * time.Second // weak this long before switching
	settleAfter = 45 * time.Second // after a switch or a network change, let the new tunnel settle
)

// Snapshot is what the controller needs to know about the app at one moment.
type Snapshot struct {
	Settings   store.Settings
	Kind       string // "wifi", "mobile", "wired"; "" = not known
	ActiveID   string
	Profiles   []store.Profile
	Up         bool  // the tunnel is up
	Answering  bool  // and the server answers
	PingMs     int64 // last check through the tunnel, -1 = no answer
	Recovering bool  // the watchdog is restoring a dead tunnel: do not interfere
}

// Host is the app the controller drives.
type Host interface {
	Snapshot() Snapshot
	// SwitchProfile makes profile id active and brings the tunnel up on it if it was up.
	SwitchProfile(id, why string) error
	// ProbeAWG checks an AmneziaWG config without the tunnel (awgprobe), in milliseconds.
	ProbeAWG(raw string) (int64, error)
	// TestServers checks servers of subscription profile id (all of them when kind is "", else
	// those the rule for kind prefers) and returns when done.
	TestServers(id, kind string)
	// ReconnectServer brings the subscription tunnel up again on the best server for the network.
	ReconnectServer(why string)
	Logf(format string, args ...any)
	Notify(title, body string)
}

// Check is the last background check of a profile.
type Check struct {
	Ms  int64     `json:"ms"` // -1 = did not work
	Err string    `json:"err,omitempty"`
	At  time.Time `json:"at"`
}

type sample struct {
	at time.Time
	ms int64
}

// Controller runs the rules above. Its methods are safe for concurrent use.
type Controller struct {
	h       Host
	working atomic.Bool // a background check is running

	mu         sync.Mutex
	kind       string // network kind last seen
	kindKnown  bool
	hold       bool // the user picked a profile by hand on this network
	samples    []sample
	sampleOf   string // profile the samples belong to
	weakSince  time.Time
	settled    time.Time // no quality decisions before this
	checks     map[string]Check
	lastCheck  map[string]time.Time // background checks of profiles (return probes)
	lastSubRun map[string]time.Time // background checks of subscription servers
	lastServer time.Time            // last switch to a faster subscription server
}

func New(h Host) *Controller {
	return &Controller{h: h, checks: map[string]Check{}, lastCheck: map[string]time.Time{}, lastSubRun: map[string]time.Time{}}
}

// Run ticks until stop closes.
func (c *Controller) Run(stop <-chan struct{}) {
	t := time.NewTicker(Tick)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		c.tick(time.Now())
	}
}

// Checks returns the last background checks by profile ID.
func (c *Controller) Checks() map[string]Check {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]Check, len(c.checks))
	for k, v := range c.checks {
		out[k] = v
	}
	return out
}

// Checking reports whether a background check runs.
func (c *Controller) Checking() bool { return c.working.Load() }

// Manual is called when the user picks a profile: it stays until the network changes.
func (c *Controller) Manual() {
	c.mu.Lock()
	c.hold = true
	c.mu.Unlock()
}

// Want returns the profile set for a kind of network, if it can be used.
func Want(s store.Settings, kind string, profiles []store.Profile) string {
	if !s.AutoProfile || kind == "" {
		return ""
	}
	id := s.NetProfiles[kind]
	if p := find(profiles, id); p != nil && p.Usable() {
		return id
	}
	return ""
}

// Fallback returns the profile to use while want is weak: the chosen fallback, else the next
// usable profile after want.
func Fallback(s store.Settings, want string, profiles []store.Profile) string {
	if p := find(profiles, s.FallbackProfile); p != nil && p.ID != want && p.Usable() {
		return p.ID
	}
	n := len(profiles)
	start := 0
	for i, p := range profiles {
		if p.ID == want {
			start = i
		}
	}
	for i := 1; i < n; i++ {
		p := profiles[(start+i)%n]
		if p.ID != want && p.Usable() {
			return p.ID
		}
	}
	return ""
}

// NextProfile is the watchdog's failover choice when the active profile is dead: with
// AutoProfile on, the fallback (or the profile for the network) first; "" leaves the plain order.
func NextProfile(s store.Settings, kind, active string, profiles []store.Profile) string {
	if !s.AutoProfile {
		return ""
	}
	want := Want(s, kind, profiles)
	if want != "" && want != active {
		return want
	}
	if fb := Fallback(s, active, profiles); fb != "" && fb != active {
		return fb
	}
	return ""
}

func find(profiles []store.Profile, id string) *store.Profile {
	if id == "" {
		return nil
	}
	for i := range profiles {
		if profiles[i].ID == id {
			return &profiles[i]
		}
	}
	return nil
}

func name(profiles []store.Profile, id string) string {
	if p := find(profiles, id); p != nil {
		return p.Name
	}
	return id
}

// NetChanged is called as soon as the platform sees a new kind of network. It returns true when
// it moved the VPN to another profile (the caller then skips its own server re-selection).
func (c *Controller) NetChanged(kind string) bool {
	return c.netChanged(c.h.Snapshot(), kind, time.Now())
}

func (c *Controller) netChanged(s Snapshot, kind string, now time.Time) bool {
	c.mu.Lock()
	if c.kindKnown && c.kind == kind {
		c.mu.Unlock()
		return false
	}
	c.kind, c.kindKnown, c.hold = kind, true, false
	c.resetSamplesLocked("")
	c.settled = now.Add(settleAfter)
	c.mu.Unlock()
	want := Want(s.Settings, kind, s.Profiles)
	if want == "" || want == s.ActiveID {
		return false
	}
	target := want
	// The profile for this network was found weak a moment ago: go straight to the fallback.
	if ck, ok := c.Checks()[want]; ok && now.Sub(ck.At) < c.every(s.Settings) && !good(ck.Ms, s.Settings) {
		if fb := Fallback(s.Settings, want, s.Profiles); fb != "" {
			target = fb
		}
	}
	if target == s.ActiveID {
		return false
	}
	why := fmt.Sprintf("сеть «%s» — профиль «%s»", netName(kind), name(s.Profiles, target))
	if target != want {
		why = fmt.Sprintf("сеть «%s» — профиль «%s» недавно работал плохо, беру запасной «%s»", netName(kind), name(s.Profiles, want), name(s.Profiles, target))
	}
	if !s.Up {
		c.h.SwitchProfile(target, why) // only selects it: the VPN is off
		return true
	}
	c.h.Notify("VPN: смена сети", fmt.Sprintf("Сеть «%s» — профиль «%s»", netName(kind), name(s.Profiles, target)))
	if err := c.h.SwitchProfile(target, why); err != nil {
		c.h.Logf("профиль «%s» не включился: %v", name(s.Profiles, target), err)
	}
	return true
}

func (c *Controller) every(s store.Settings) time.Duration {
	if s.CheckEveryMin <= 0 {
		return 10 * time.Minute
	}
	return time.Duration(s.CheckEveryMin) * time.Minute
}

func weakLimit(s store.Settings) int64 {
	if s.WeakPingMs <= 0 {
		return 300
	}
	return int64(s.WeakPingMs)
}

// good: a check result to come back to a profile for (well below the weak limit).
func good(ms int64, s store.Settings) bool {
	return ms > 0 && ms*10 < weakLimit(s)*8
}

func (c *Controller) resetSamplesLocked(id string) {
	c.samples, c.sampleOf, c.weakSince = nil, id, time.Time{}
}

// Weak tells whether ping samples show a weak connection: half of them lost, or the median
// delay above limit. med is that median (-1 when nothing answered).
func Weak(ms []int64, limit int64) (weak bool, med int64) {
	if len(ms) < minSamples {
		return false, -1
	}
	var ok []int64
	for _, v := range ms {
		if v > 0 {
			ok = append(ok, v)
		}
	}
	if len(ok)*2 <= len(ms) {
		return true, -1
	}
	sort.Slice(ok, func(i, j int) bool { return ok[i] < ok[j] })
	med = ok[len(ok)/2]
	return med > limit, med
}

func (c *Controller) tick(now time.Time) {
	s := c.h.Snapshot()
	if s.Kind != "" {
		c.mu.Lock()
		changed := !c.kindKnown || c.kind != s.Kind
		c.mu.Unlock()
		if changed && c.netChanged(s, s.Kind, now) {
			return
		}
	}
	if !s.Up {
		c.mu.Lock()
		c.resetSamplesLocked("")
		c.mu.Unlock()
		return
	}
	c.backgroundSubs(s, now)

	// ping samples of the active profile
	c.mu.Lock()
	if c.sampleOf != s.ActiveID {
		c.resetSamplesLocked(s.ActiveID)
		c.settled = now.Add(settleAfter)
	}
	if !now.Before(c.settled) {
		c.samples = append(c.samples, sample{now, s.PingMs})
	}
	cut := 0
	for cut < len(c.samples) && now.Sub(c.samples[cut].at) > window {
		cut++
	}
	c.samples = c.samples[cut:]
	ms := make([]int64, len(c.samples))
	for i, x := range c.samples {
		ms[i] = x.ms
	}
	weak, med := Weak(ms, weakLimit(s.Settings))
	if !weak || s.Recovering || now.Before(c.settled) {
		c.weakSince = time.Time{}
	} else if c.weakSince.IsZero() {
		c.weakSince = now
	}
	weakLong := !c.weakSince.IsZero() && now.Sub(c.weakSince) >= weakFor
	hold := c.hold
	c.mu.Unlock()
	if s.Recovering {
		return
	}

	active := find(s.Profiles, s.ActiveID)
	if active == nil {
		return
	}
	want := Want(s.Settings, s.Kind, s.Profiles)
	quality := "потеряна половина проверок"
	if med > 0 {
		quality = fmt.Sprintf("задержка %d мс", med)
	}

	if weakLong {
		// A weak subscription server: a clearly faster one of the same subscription first.
		if active.Sub != nil && c.fasterServer(s, active, med, now) {
			c.mu.Lock()
			c.lastServer = now
			c.resetSamplesLocked(s.ActiveID)
			c.settled = now.Add(settleAfter)
			c.mu.Unlock()
			c.h.ReconnectServer(fmt.Sprintf("сервер работает плохо (%s) — перехожу на более быстрый", quality))
			return
		}
		if want != "" && want == s.ActiveID && !hold {
			if fb := Fallback(s.Settings, want, s.Profiles); fb != "" {
				c.mu.Lock()
				c.checks[want] = Check{Ms: med, Err: "слабая связь", At: now}
				c.lastCheck[want] = now
				c.resetSamplesLocked("")
				c.mu.Unlock()
				why := fmt.Sprintf("профиль «%s» работает плохо (%s) — переключаюсь на «%s»", active.Name, quality, name(s.Profiles, fb))
				c.h.Notify("VPN: запасной профиль", why)
				if err := c.h.SwitchProfile(fb, why); err != nil {
					c.h.Logf("запасной профиль не включился: %v", err)
				}
				return
			}
		}
	}

	// On the fallback (or after a failover): check the profile for the network in the background
	// and take it back when it answers well.
	if want == "" || want == s.ActiveID || hold || s.Settings.CheckEveryMin <= 0 {
		return
	}
	c.mu.Lock()
	due := now.Sub(c.lastCheck[want]) >= c.every(s.Settings)
	c.mu.Unlock()
	if !due || !c.working.CompareAndSwap(false, true) {
		return // a server check runs: try on the next tick
	}
	c.mu.Lock()
	c.lastCheck[want] = now
	c.mu.Unlock()
	go func() {
		defer c.working.Store(false)
		ck := c.checkProfile(s, want)
		if !good(ck.Ms, s.Settings) {
			c.h.Logf("фоновая проверка: профиль «%s» — %s, остаюсь на «%s»", name(s.Profiles, want), checkText(ck), active.Name)
			return
		}
		cur := c.h.Snapshot()
		c.mu.Lock()
		stillOK := cur.ActiveID == s.ActiveID && c.kind == s.Kind && !c.hold
		c.mu.Unlock()
		if !stillOK || !cur.Up {
			return
		}
		why := fmt.Sprintf("профиль «%s» снова работает хорошо (%d мс) — возвращаюсь на него", name(s.Profiles, want), ck.Ms)
		c.h.Notify("VPN: основной профиль", why)
		if err := c.h.SwitchProfile(want, why); err != nil {
			c.h.Logf("профиль «%s» не включился: %v", name(s.Profiles, want), err)
		}
	}()
}

func checkText(ck Check) string {
	if ck.Ms > 0 {
		return fmt.Sprintf("%d мс", ck.Ms)
	}
	if ck.Err != "" {
		return ck.Err
	}
	return "нет ответа"
}

// CheckProfile checks a profile that does not carry the traffic now and records the result.
func (c *Controller) CheckProfile(id string) (Check, error) {
	s := c.h.Snapshot()
	if find(s.Profiles, id) == nil {
		return Check{}, fmt.Errorf("профиль не найден")
	}
	if id == s.ActiveID && s.Up {
		return c.checkProfile(s, id), nil // the tunnel's own delay: no probe to wait for
	}
	if !c.working.CompareAndSwap(false, true) {
		return Check{}, fmt.Errorf("проверка уже идёт")
	}
	defer c.working.Store(false)
	return c.checkProfile(s, id), nil
}

func (c *Controller) checkProfile(s Snapshot, id string) Check {
	p := find(s.Profiles, id)
	ck := Check{Ms: -1, At: time.Now()}
	switch {
	case p == nil:
		ck.Err = "профиль не найден"
	case id == s.ActiveID && s.Up:
		ck.Ms = s.PingMs // the tunnel's own checks
		if ck.Ms <= 0 {
			ck.Err = "нет ответа"
		}
	case p.Sub != nil:
		c.h.TestServers(id, s.Kind)
		ck.Ms, ck.Err = c.bestServer(id, s.Kind)
	case p.RawConfig != "":
		ms, err := c.h.ProbeAWG(p.RawConfig)
		ck.Ms = ms
		if err != nil {
			ck.Ms, ck.Err = -1, err.Error()
		}
	default:
		ck.Err = "нет конфига"
	}
	ck.At = time.Now()
	c.mu.Lock()
	c.checks[id] = ck
	c.mu.Unlock()
	return ck
}

// bestServer is the fastest fresh delay among the servers the rule for kind prefers.
func (c *Controller) bestServer(id, kind string) (int64, string) {
	s := c.h.Snapshot()
	p := find(s.Profiles, id)
	if p == nil || p.Sub == nil {
		return -1, "профиль не найден"
	}
	if kind == "" {
		kind = subscription.NetWired
	}
	best := int64(-1)
	for _, srv := range p.Sub.Preferred(kind) {
		if m, ok := p.Sub.Measures[srv.ID]; ok && m.Ms > 0 && time.Since(m.At) < subscription.Fresh && (best < 0 || m.Ms < best) {
			best = m.Ms
		}
	}
	if best < 0 {
		return -1, "ни один сервер не ответил"
	}
	return best, ""
}

// fasterServer reports whether the subscription has a server the rule allows that answered
// clearly faster than the current one in a fresh check (at most one such switch per interval).
func (c *Controller) fasterServer(s Snapshot, p *store.Profile, med int64, now time.Time) bool {
	c.mu.Lock()
	recent := now.Sub(c.lastServer) < c.every(s.Settings)
	c.mu.Unlock()
	if recent {
		return false
	}
	kind := s.Kind
	if kind == "" {
		kind = subscription.NetWired
	}
	rule := p.Sub.Rule(kind)
	if subscription.FixedID(rule) != "" {
		return false // the user chose this server
	}
	for _, srv := range p.Sub.Preferred(kind) {
		if srv.ID == p.Sub.Current {
			continue
		}
		m, ok := p.Sub.Measures[srv.ID]
		if ok && m.Ms > 0 && now.Sub(m.At) < subscription.Fresh && (med <= 0 || m.Ms*2 < med) && m.Ms < weakLimit(s.Settings) {
			return true
		}
	}
	return false
}

// backgroundSubs checks the servers of the subscriptions in play every CheckEveryMin minutes.
func (c *Controller) backgroundSubs(s Snapshot, now time.Time) {
	if s.Settings.CheckEveryMin <= 0 {
		return
	}
	every := c.every(s.Settings)
	inPlay := map[string]bool{s.ActiveID: true}
	if s.Settings.AutoProfile {
		for _, id := range s.Settings.NetProfiles {
			inPlay[id] = true
		}
		inPlay[Fallback(s.Settings, Want(s.Settings, s.Kind, s.Profiles), s.Profiles)] = true
	}
	var due []string
	c.mu.Lock()
	for _, p := range s.Profiles {
		if !inPlay[p.ID] || p.Sub == nil || len(p.Sub.Servers) == 0 {
			continue
		}
		last := c.lastSubRun[p.ID]
		if last.IsZero() { // first time: start counting from now, a check ran on connect
			c.lastSubRun[p.ID] = now
			continue
		}
		if now.Sub(last) >= every {
			c.lastSubRun[p.ID] = now
			due = append(due, p.ID)
		}
	}
	c.mu.Unlock()
	if len(due) == 0 || !c.working.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer c.working.Store(false)
		for _, id := range due {
			c.h.TestServers(id, "")
		}
	}()
}

func netName(kind string) string {
	switch kind {
	case subscription.NetWiFi:
		return "Wi-Fi"
	case subscription.NetMobile:
		return "мобильная"
	case subscription.NetWired:
		return "кабель"
	}
	return "неизвестная"
}

// Roles lists what the profile is chosen for: kinds of network and "fallback".
func Roles(s store.Settings, id string) []string {
	if !s.AutoProfile || id == "" {
		return nil
	}
	var out []string
	for _, k := range subscription.NetKinds {
		if s.NetProfiles[k] == id {
			out = append(out, k)
		}
	}
	if s.FallbackProfile == id {
		out = append(out, "fallback")
	}
	return out
}
