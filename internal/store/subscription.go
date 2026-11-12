package store

import (
	"time"

	"awgpilot/internal/subscription"
)

// Subscription is a proxy subscription with its servers and how to choose among them.
type Subscription struct {
	// URL is the subscription link; empty for servers added by their own links.
	URL     string                `json:"url,omitempty"`
	Info    subscription.Info     `json:"info"`
	Servers []subscription.Server `json:"servers"`
	Updated time.Time             `json:"updated"`
	// Rules choose servers per kind of network ("wifi", "mobile", "wired"); a missing kind uses
	// subscription.DefaultRule.
	Rules map[string]string `json:"rules,omitempty"`
	// Measures are the last checks of the servers, by server ID.
	Measures map[string]subscription.Measure `json:"measures,omitempty"`
	// Current is the server the tunnel last went through.
	Current string `json:"current,omitempty"`
}

// Rule returns the rule for a kind of network.
func (s *Subscription) Rule(kind string) string {
	if r := s.Rules[kind]; r != "" {
		if id := subscription.FixedID(r); id == "" || subscription.Find(s.Servers, id) != nil {
			return r
		}
	}
	return subscription.DefaultRule(kind, s.Servers)
}

// Merge takes a fresh server list from the provider, keeping the user's rules and the checks of
// the servers that are still there. name is the profile's name; Merge returns the name the
// provider suggests now, or "" to keep it (it was chosen by the user).
func (s *Subscription) Merge(info subscription.Info, servers []subscription.Server, name string) string {
	measures := map[string]subscription.Measure{}
	for _, srv := range servers {
		if m, ok := s.Measures[srv.ID]; ok {
			measures[srv.ID] = m
		}
	}
	rename := ""
	if info.Title != "" && (name == s.Info.Title || name == "") {
		rename = info.Title
	}
	s.Info, s.Servers, s.Measures, s.Updated = info, servers, measures, time.Now()
	return rename
}

// Pick chooses the server for a kind of network: the current one while the rule still wants it
// and it has not failed, otherwise the best candidate that has not failed in this round. Servers
// that a fresh check found dead come only when nothing else is left.
func (s *Subscription) Pick(kind string, failed map[string]bool) (subscription.Server, bool) {
	rule := s.Rule(kind)
	now := time.Now()
	deadNow := func(id string) bool {
		m, ok := s.Measures[id]
		return ok && m.Ms <= 0 && now.Sub(m.At) < subscription.Fresh
	}
	if cur := subscription.Find(s.Servers, s.Current); cur != nil && !failed[cur.ID] && !deadNow(cur.ID) && subscription.Prefers(rule, *cur) {
		return *cur, true
	}
	cands := subscription.Candidates(s.Servers, rule, s.Measures, now)
	for _, srv := range cands {
		if !failed[srv.ID] && !deadNow(srv.ID) {
			return srv, true
		}
	}
	for _, srv := range cands {
		if !failed[srv.ID] {
			return srv, true
		}
	}
	return subscription.Server{}, false
}

// Preferred lists the servers the rule for kind looks among first.
func (s *Subscription) Preferred(kind string) []subscription.Server {
	rule := s.Rule(kind)
	var out []subscription.Server
	for _, srv := range s.Servers {
		if subscription.Prefers(rule, srv) {
			out = append(out, srv)
		}
	}
	return out
}

// SetMeasure records a check of a server.
func (s *Subscription) SetMeasure(id string, m subscription.Measure) {
	if s.Measures == nil {
		s.Measures = map[string]subscription.Measure{}
	}
	s.Measures[id] = m
}
