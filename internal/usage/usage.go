// Package usage keeps per-day traffic totals through the tunnel.
package usage

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"sync"
	"time"
)

const (
	dayLayout = "2006-01-02"
	keepDays  = 400
)

type Day struct {
	Rx uint64 `json:"rx"`
	Tx uint64 `json:"tx"`
}

type DayPoint struct {
	Date string `json:"date"`
	Rx   uint64 `json:"rx"`
	Tx   uint64 `json:"tx"`
}

type Summary struct {
	Today  Day        `json:"today"`
	Week   Day        `json:"week"`  // last 7 days including today
	Month  Day        `json:"month"` // current calendar month
	All    Day        `json:"all"`
	Since  string     `json:"since"` // first recorded day
	Last30 []DayPoint `json:"last30"`
}

type Store struct {
	mu    sync.Mutex
	path  string
	days  map[string]*Day
	dirty bool
}

// Open loads totals from path; a missing or unreadable file starts empty.
func Open(path string) *Store {
	s := &Store{path: path, days: map[string]*Day{}}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &s.days)
		if s.days == nil {
			s.days = map[string]*Day{}
		}
	}
	return s
}

// Add attributes traffic to the local calendar day of t.
func (s *Store) Add(t time.Time, rx, tx uint64) {
	if rx == 0 && tx == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := t.Local().Format(dayLayout)
	d := s.days[k]
	if d == nil {
		d = &Day{}
		s.days[k] = d
	}
	d.Rx += rx
	d.Tx += tx
	s.dirty = true
}

// Save writes the file when something changed, dropping days older than keepDays.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty || s.path == "" {
		return nil
	}
	cutoff := time.Now().AddDate(0, 0, -keepDays).Format(dayLayout)
	for k := range s.days {
		if k < cutoff {
			delete(s.days, k)
		}
	}
	b, err := json.Marshal(s.days)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

// Reset forgets all totals.
func (s *Store) Reset() error {
	s.mu.Lock()
	s.days = map[string]*Day{}
	s.dirty = true
	s.mu.Unlock()
	err := s.Save()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *Store) Summary(now time.Time) Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	now = now.Local()
	today := now.Format(dayLayout)
	weekFrom := now.AddDate(0, 0, -6).Format(dayLayout)
	monthPrefix := now.Format("2006-01")
	var sum Summary
	keys := make([]string, 0, len(s.days))
	for k := range s.days {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		sum.Since = keys[0]
	}
	add := func(a *Day, d *Day) { a.Rx += d.Rx; a.Tx += d.Tx }
	for _, k := range keys {
		d := s.days[k]
		add(&sum.All, d)
		if k == today {
			add(&sum.Today, d)
		}
		if k >= weekFrom && k <= today {
			add(&sum.Week, d)
		}
		if len(k) >= 7 && k[:7] == monthPrefix {
			add(&sum.Month, d)
		}
	}
	for i := 29; i >= 0; i-- {
		k := now.AddDate(0, 0, -i).Format(dayLayout)
		p := DayPoint{Date: k}
		if d := s.days[k]; d != nil {
			p.Rx, p.Tx = d.Rx, d.Tx
		}
		sum.Last30 = append(sum.Last30, p)
	}
	return sum
}
