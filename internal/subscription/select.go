package subscription

import (
	"sort"
	"strings"
	"time"
)

// Kinds of network the computer or phone is on; the rules pick servers per kind.
const (
	NetWiFi   = "wifi"
	NetMobile = "mobile"
	NetWired  = "wired"
)

// NetKinds in the order the UI shows them.
var NetKinds = []string{NetWiFi, NetMobile, NetWired}

// A rule says which servers to use on a kind of network:
//
//	"auto"       the fastest server of the subscription
//	"auto:LTE"   the fastest server of the group LTE
//	"auto:!LTE"  the fastest server outside the group LTE
//	"server:ID"  that server; the others (fastest first) only if it does not work
//
// Whatever the rule, when its servers stop working the others are tried too.
const RuleAuto = "auto"

// Measure is the last check of a server.
type Measure struct {
	Ms  int64     `json:"ms"` // -1 = did not work
	Err string    `json:"err,omitempty"`
	At  time.Time `json:"at"`
}

// Fresh is how long a measurement is trusted when choosing a server.
const Fresh = 20 * time.Minute

// Groups returns the groups present among servers, in order of first appearance.
func Groups(servers []Server) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range servers {
		if s.Group != "" && !seen[s.Group] {
			seen[s.Group] = true
			out = append(out, s.Group)
		}
	}
	return out
}

func hasGroup(servers []Server, g string) bool {
	for _, s := range servers {
		if s.Group == g {
			return true
		}
	}
	return false
}

// DefaultRule is the rule for a network when the user has not chosen one: providers that mark
// servers for mobile networks ("LTE") mean them for mobile internet, where other servers are often
// blocked, and the others for Wi-Fi and cable.
func DefaultRule(kind string, servers []Server) string {
	if !hasGroup(servers, "LTE") {
		return RuleAuto
	}
	if kind == NetMobile {
		return "auto:LTE"
	}
	return "auto:!LTE"
}

// matches reports whether s is one of the servers the rule asks for first.
func matches(rule string, s Server) bool {
	g, ok := strings.CutPrefix(rule, "auto:")
	if !ok {
		return true
	}
	if not, ok := strings.CutPrefix(g, "!"); ok {
		return s.Group != not
	}
	return s.Group == g
}

// FixedID returns the server of a "server:ID" rule.
func FixedID(rule string) string {
	id, _ := strings.CutPrefix(rule, "server:")
	if id == rule {
		return ""
	}
	return id
}

// Candidates orders all servers for trying on a network: the servers the rule asks for first (a
// fixed server before everything), each part fastest first, untested next, failed last.
func Candidates(servers []Server, rule string, m map[string]Measure, now time.Time) []Server {
	if rule == "" {
		rule = RuleAuto
	}
	fixed := FixedID(rule)
	var first, rest []Server
	for _, s := range servers {
		switch {
		case s.ID == fixed:
			// put in front below
		case fixed == "" && matches(rule, s):
			first = append(first, s)
		default:
			rest = append(rest, s)
		}
	}
	rank := func(list []Server) {
		sort.SliceStable(list, func(i, j int) bool {
			ri, rj := score(m[list[i].ID], now), score(m[list[j].ID], now)
			return ri < rj
		})
	}
	rank(first)
	rank(rest)
	var out []Server
	for _, s := range servers {
		if s.ID == fixed {
			out = append(out, s)
		}
	}
	return append(append(out, first...), rest...)
}

// score sorts servers: working ones by delay, then untested or stale, then failed.
func score(x Measure, now time.Time) int64 {
	switch {
	case x.At.IsZero():
		return 1 << 40
	case x.Ms > 0 && now.Sub(x.At) < 2*Fresh:
		return x.Ms
	case x.Ms > 0:
		return 1<<40 - 1 // worked a while ago
	default:
		return 1<<41 - now.Sub(x.At).Milliseconds() // failed; the longest ago first
	}
}

// NeedTest reports whether the servers that the rule prefers lack fresh measurements, so a
// choice would be a guess.
func NeedTest(servers []Server, rule string, m map[string]Measure, now time.Time) bool {
	fixed := FixedID(rule)
	for _, s := range servers {
		if fixed != "" && s.ID != fixed {
			continue
		}
		if fixed == "" && !matches(rule, s) {
			continue
		}
		if x, ok := m[s.ID]; !ok || now.Sub(x.At) > Fresh {
			return true
		}
	}
	return false
}

// Find returns the server with id.
func Find(servers []Server, id string) *Server {
	for i := range servers {
		if servers[i].ID == id {
			return &servers[i]
		}
	}
	return nil
}

// Prefers reports whether the rule asks for s first: it is the fixed server, or it is one the
// automatic choice looks among. A working server that the rule prefers is kept on reconnects.
func Prefers(rule string, s Server) bool {
	if fixed := FixedID(rule); fixed != "" {
		return s.ID == fixed
	}
	return matches(rule, s)
}
