// Package flows records which programs send traffic through the tunnel and where: the tunnel
// device hands every packet to Recorder.Packet, which counts bytes per connection, learns host
// names from DNS answers and TLS/HTTP requests, and folds the connections into per-day totals
// by program and destination. The days are kept as small JSON files (about three years);
// reports cover one day or a period of days.
package flows

import (
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	dayLayout  = "2006-01-02"
	keepDays   = 1100  // "all time" in practice; a day file is tens of kilobytes
	maxReport  = 5000  // rows sent to the window for a period
	maxFlows   = 50000 // live connections tracked at once; beyond this new ones are not counted
	maxNames   = 20000 // IP -> name entries learned from DNS
	maxRows    = 20000 // rows per day; beyond this traffic goes to one "other" row
	idleAfter  = 2 * time.Minute
	helloBytes = 8192 // enough of a ClientHello to reach server_name even with post-quantum key shares
)

// Unknown labels traffic whose program could not be found (the connection ended too quickly).
const Unknown = "неизвестная программа"

// OwnerFunc names the program that owns a connection of the tunnel ("" when not found).
type OwnerFunc func(proto uint8, local netip.Addr, lport uint16, remote netip.Addr, rport uint16) string

type key struct {
	proto         uint8
	local, remote netip.Addr
	rport, lport  uint16
}

type flow struct {
	tx, rx   uint64 // not yet folded into the day
	app      string
	resolved bool
	tries    int
	host     string // from SNI or the HTTP Host header
	hello    []byte // first outgoing TCP payload, while looking for a host name
	segs     int
	counted  string // day this connection was last counted in Row.Conns
	seen     time.Time
	born     time.Time
}

// Row is the traffic of one program to one destination on one day.
type Row struct {
	App   string `json:"app"`
	Host  string `json:"host"` // name when known, otherwise the IP address
	Port  uint16 `json:"port"`
	Proto string `json:"proto"`
	Tx    uint64 `json:"tx"`
	Rx    uint64 `json:"rx"`
	Conns int    `json:"conns"`
	Last  int64  `json:"last"` // unix seconds
}

type rowKey struct {
	app, host, proto string
	port             uint16
}

// Report is what the window shows for one day.
type Report struct {
	Enabled bool     `json:"enabled"`
	Day     string   `json:"day"`
	Days    []string `json:"days"` // days with a record, newest first
	Rows    []Row    `json:"rows"`
}

type Recorder struct {
	dir   string
	owner OwnerFunc
	on    atomic.Bool
	wake  chan struct{}

	mu    sync.Mutex
	flows map[key]*flow
	names map[netip.Addr]string
	day   string
	rows  map[rowKey]*Row
	dirty bool

	// past caches the sum of finished days for a period: those files no longer change.
	cacheMu sync.Mutex
	past    map[string]map[rowKey]Row
}

// New loads today's record from dir. owner may be nil (programs are then unknown).
func New(dir string, owner OwnerFunc) *Recorder {
	r := &Recorder{dir: dir, owner: owner, wake: make(chan struct{}, 1),
		flows: map[key]*flow{}, names: map[netip.Addr]string{}}
	_, err := os.Stat(filepath.Join(dir, "enabled"))
	r.on.Store(err == nil)
	r.openDay(time.Now().Format(dayLayout))
	return r
}

func (r *Recorder) Enabled() bool { return r.on.Load() }

// SetEnabled turns recording on or off; the choice survives restarts.
func (r *Recorder) SetEnabled(on bool) error {
	r.on.Store(on)
	flag := filepath.Join(r.dir, "enabled")
	if !on {
		r.mu.Lock()
		r.flows = map[key]*flow{}
		r.mu.Unlock()
		if err := os.Remove(flag); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(flag, []byte("1"), 0o644)
}

// ---- packet path --------------------------------------------------------------------------

// Packet counts one IP packet; out is true for packets leaving the computer into the tunnel.
func (r *Recorder) Packet(pkt []byte, out bool) {
	if !r.on.Load() {
		return
	}
	p, ok := parse(pkt)
	if !ok {
		return
	}
	k := key{proto: p.proto}
	if out {
		k.local, k.remote, k.rport, k.lport = p.src, p.dst, p.dport, p.sport
	} else {
		k.local, k.remote, k.rport, k.lport = p.dst, p.src, p.sport, p.dport
	}
	if !out && p.proto == protoUDP && p.sport == 53 {
		r.learnDNS(p.payload)
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.flows[k]
	if f == nil {
		if len(r.flows) >= maxFlows {
			return
		}
		f = &flow{born: now}
		if p.proto != protoTCP && p.proto != protoUDP {
			f.app, f.resolved = "ICMP / прочее", true
		}
		r.flows[k] = f
		select {
		case r.wake <- struct{}{}:
		default:
		}
	}
	f.seen = now
	if out {
		f.tx += uint64(len(pkt))
		if p.proto == protoTCP && f.host == "" && f.segs < 4 && len(p.payload) > 0 {
			f.segs++
			f.lookForHost(p.payload)
		}
	} else {
		f.rx += uint64(len(pkt))
	}
}

func (f *flow) lookForHost(payload []byte) {
	if len(f.hello)+len(payload) > helloBytes {
		payload = payload[:helloBytes-len(f.hello)]
	}
	f.hello = append(f.hello, payload...)
	host, done := tlsServerName(f.hello)
	if done && host == "" {
		host, done = httpHost(f.hello)
	}
	if host != "" {
		f.host = strings.ToLower(host)
	}
	if done || f.segs >= 4 || len(f.hello) >= helloBytes {
		f.hello = nil
		f.segs = 4
	}
}

func (r *Recorder) learnDNS(msg []byte) {
	answers := dnsAnswers(msg)
	if len(answers) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.names)+len(answers) > maxNames {
		r.names = map[netip.Addr]string{}
	}
	for a, n := range answers {
		r.names[a] = n
	}
}

// ---- background work ----------------------------------------------------------------------

// Run finds programs for new connections, folds counters into the day and saves it, until stop
// is closed (then it saves once more).
func (r *Recorder) Run(stop <-chan struct{}) {
	fold := time.NewTicker(10 * time.Second)
	retry := time.NewTicker(time.Second)
	defer fold.Stop()
	defer retry.Stop()
	lastSave := time.Now()
	for {
		select {
		case <-stop:
			r.fold(time.Now())
			r.save()
			return
		case <-r.wake:
			time.Sleep(30 * time.Millisecond) // let a burst of new connections arrive together
			r.resolve()
		case <-retry.C:
			r.resolve()
		case now := <-fold.C:
			r.fold(now)
			if now.Sub(lastSave) >= time.Minute {
				lastSave = now
				r.save()
			}
		}
	}
}

type pending struct {
	k key
	f *flow
}

// resolve asks the owner lookup about connections without a program. It runs outside the lock:
// the lookup reads the system connection tables, which takes milliseconds.
func (r *Recorder) resolve() {
	if r.owner == nil || !r.on.Load() {
		return
	}
	var todo []pending
	r.mu.Lock()
	for k, f := range r.flows {
		if !f.resolved {
			todo = append(todo, pending{k, f})
		}
	}
	r.mu.Unlock()
	if len(todo) == 0 {
		return
	}
	names := make([]string, len(todo))
	for i, t := range todo {
		names[i] = r.owner(t.k.proto, t.k.local, t.k.lport, t.k.remote, t.k.rport)
	}
	r.mu.Lock()
	for i, t := range todo {
		switch {
		case names[i] != "":
			t.f.app, t.f.resolved = names[i], true
		default:
			if t.f.tries++; t.f.tries >= 3 {
				t.f.app, t.f.resolved = Unknown, true
			}
		}
	}
	r.mu.Unlock()
}

// fold moves the counted bytes into the day's rows and forgets idle connections.
func (r *Recorder) fold(now time.Time) {
	day := now.Format(dayLayout)
	r.mu.Lock()
	defer r.mu.Unlock()
	if day != r.day {
		r.saveLocked()
		r.openDayLocked(day)
	}
	for k, f := range r.flows {
		if (f.tx > 0 || f.rx > 0) && (f.resolved || now.Sub(f.born) > 5*time.Second) {
			app := f.app
			if app == "" {
				app = Unknown
			}
			rk := rowKey{app: app, host: r.hostLocked(k, f), proto: protoName(k.proto), port: k.rport}
			row := r.rows[rk]
			if row == nil {
				if len(r.rows) >= maxRows {
					rk = rowKey{app: app, host: "прочие адреса", proto: rk.proto}
				}
				if row = r.rows[rk]; row == nil {
					row = &Row{App: rk.app, Host: rk.host, Port: rk.port, Proto: rk.proto}
					r.rows[rk] = row
				}
			}
			row.Tx += f.tx
			row.Rx += f.rx
			if f.counted != day {
				f.counted = day
				row.Conns++
			}
			if t := f.seen.Unix(); t > row.Last {
				row.Last = t
			}
			f.tx, f.rx = 0, 0
			r.dirty = true
		}
		if now.Sub(f.seen) > idleAfter && f.tx == 0 && f.rx == 0 {
			delete(r.flows, k)
		}
	}
}

func (r *Recorder) hostLocked(k key, f *flow) string {
	if f.host != "" {
		return f.host
	}
	if n := r.names[k.remote]; n != "" {
		return n
	}
	return k.remote.String()
}

func protoName(p uint8) string {
	switch p {
	case protoTCP:
		return "TCP"
	case protoUDP:
		return "UDP"
	case 1, 58:
		return "ICMP"
	}
	return "IP"
}

// ---- storage ------------------------------------------------------------------------------

func (r *Recorder) path(day string) string { return filepath.Join(r.dir, day+".json") }

func (r *Recorder) openDay(day string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.openDayLocked(day)
}

func (r *Recorder) openDayLocked(day string) {
	r.day, r.rows, r.dirty = day, map[rowKey]*Row{}, false
	for _, row := range readDay(r.path(day)) {
		row := row
		r.rows[rowKey{row.App, row.Host, row.Proto, row.Port}] = &row
	}
}

func readDay(path string) []Row {
	var rows []Row
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &rows)
	}
	return rows
}

func (r *Recorder) save() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saveLocked()
}

func (r *Recorder) saveLocked() {
	if !r.dirty || r.dir == "" {
		return
	}
	if os.MkdirAll(r.dir, 0o755) != nil {
		return
	}
	b, err := json.Marshal(r.sortedLocked())
	if err != nil {
		return
	}
	tmp := r.path(r.day) + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil && os.Rename(tmp, r.path(r.day)) == nil {
		r.dirty = false
	}
	cutoff := time.Now().AddDate(0, 0, -keepDays).Format(dayLayout)
	for _, d := range r.listDays() {
		if d < cutoff {
			os.Remove(r.path(d))
		}
	}
}

func (r *Recorder) sortedLocked() []Row {
	out := make([]Row, 0, len(r.rows))
	for _, row := range r.rows {
		out = append(out, *row)
	}
	sortRows(out)
	return out
}

func sortRows(rows []Row) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i].Tx+rows[i].Rx, rows[j].Tx+rows[j].Rx
		if a != b {
			return a > b
		}
		return rows[i].App+rows[i].Host < rows[j].App+rows[j].Host
	})
}

func (r *Recorder) listDays() []string {
	ents, _ := os.ReadDir(r.dir)
	var days []string
	for _, e := range ents {
		n := e.Name()
		if d, ok := strings.CutSuffix(n, ".json"); ok {
			if _, err := time.Parse(dayLayout, d); err == nil {
				days = append(days, d)
			}
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(days)))
	return days
}

// Periods a report can cover besides a single day: how many days back, today included.
var periods = map[string]int{"week": 7, "month": 30, "6m": 182, "12m": 365, "all": keepDays + 1}

// Report returns the rows of one day (YYYY-MM-DD, "" = today) or of a period (week, month,
// 6m, 12m, all); today includes traffic counted a moment ago.
func (r *Recorder) Report(day string) Report {
	now := time.Now()
	today := now.Format(dayLayout)
	r.fold(now)
	rep := Report{Enabled: r.on.Load(), Day: day}
	days := r.listDays()
	if n, ok := periods[day]; ok {
		rep.Rows = r.periodRows(day, n, today, days)
	} else {
		if _, err := time.Parse(dayLayout, day); err != nil {
			day, rep.Day = today, ""
		}
		r.mu.Lock()
		if day == r.day {
			rep.Rows = r.sortedLocked()
		}
		r.mu.Unlock()
		if rep.Rows == nil {
			rep.Rows = readDay(r.path(day))
			sortRows(rep.Rows)
		}
	}
	rep.Days = days
	if today := now.Format(dayLayout); len(rep.Days) == 0 || rep.Days[0] != today {
		rep.Days = append([]string{today}, rep.Days...)
	}
	return rep
}

// periodRows adds today's live rows to the cached sum of the earlier days of the period.
func (r *Recorder) periodRows(period string, n int, today string, days []string) []Row {
	from, _ := time.Parse(dayLayout, today)
	first := from.AddDate(0, 0, -(n - 1)).Format(dayLayout)
	key := period + "|" + today
	r.cacheMu.Lock()
	past, ok := r.past[key]
	if !ok {
		past = map[rowKey]Row{}
		for _, d := range days {
			if d >= first && d < today {
				for _, row := range readDay(r.path(d)) {
					addRow(past, row)
				}
			}
		}
		if r.past == nil || len(r.past) > 8 {
			r.past = map[string]map[rowKey]Row{}
		}
		r.past[key] = past
	}
	sum := make(map[rowKey]Row, len(past))
	for k, v := range past {
		sum[k] = v
	}
	r.cacheMu.Unlock()
	r.mu.Lock()
	if r.day == today {
		for _, row := range r.rows {
			addRow(sum, *row)
		}
	}
	r.mu.Unlock()
	out := make([]Row, 0, len(sum))
	for _, row := range sum {
		out = append(out, row)
	}
	sortRows(out)
	if len(out) > maxReport {
		out = out[:maxReport]
	}
	return out
}

func addRow(m map[rowKey]Row, row Row) {
	k := rowKey{row.App, row.Host, row.Proto, row.Port}
	cur, ok := m[k]
	if !ok {
		m[k] = row
		return
	}
	cur.Tx += row.Tx
	cur.Rx += row.Rx
	cur.Conns += row.Conns
	if row.Last > cur.Last {
		cur.Last = row.Last
	}
	m[k] = cur
}

// Clear deletes every recorded day.
func (r *Recorder) Clear() error {
	r.cacheMu.Lock()
	r.past = nil
	r.cacheMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range r.listDays() {
		if err := os.Remove(r.path(d)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	for _, f := range r.flows {
		f.tx, f.rx, f.counted = 0, 0, ""
	}
	r.rows, r.dirty = map[rowKey]*Row{}, false
	return nil
}
