// Package subui describes a proxy subscription to the interface (Windows and Android share it).
// The server links, which hold the user's credentials, never leave the core.
package subui

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"awgpilot/internal/i18n"
	"awgpilot/internal/store"
	"awgpilot/internal/subscription"
)

// View is the active subscription as the UI shows it.
type View struct {
	HasURL      bool              `json:"has_url"`
	Host        string            `json:"host"`
	Title       string            `json:"title"`
	Announce    string            `json:"announce"`
	SupportURL  string            `json:"support_url"`
	WebPage     string            `json:"web_page"`
	Used        int64             `json:"used"`
	Total       int64             `json:"total"`
	Expire      int64             `json:"expire"`
	Updated     time.Time         `json:"updated"`
	UpdateHours int               `json:"update_hours"`
	Groups      []string          `json:"groups"`
	Servers     []Server          `json:"servers"`
	Rules       map[string]string `json:"rules"`    // in effect, defaults included
	Defaults    map[string]string `json:"defaults"` // what each network uses without a choice
	Custom      map[string]string `json:"custom"`   // chosen by the user
	Current     string            `json:"current"`  // server in use or last used
	NetKind     string            `json:"net_kind"` // network the device is on now
	Testing     bool              `json:"testing"`
	Tested      int               `json:"tested"`
	TestTotal   int               `json:"test_total"`
	TestStarted time.Time         `json:"test_started"` // servers checked before this are being checked again
}

// Server is one server of the view.
type Server struct {
	ID    string    `json:"id"`
	Name  string    `json:"name"`
	Group string    `json:"group"`
	Proto string    `json:"proto"`
	Host  string    `json:"host"`
	Port  int       `json:"port"`
	Ms    int64     `json:"ms"` // 0 = not checked, -1 = failed
	Err   string    `json:"err"`
	At    time.Time `json:"at"`
}

// Progress of a running server check.
type Progress struct {
	Testing       bool
	Tested, Total int
	Started       time.Time
}

// New builds the view of s for a device on a network of kind.
func New(s *store.Subscription, kind string, pr Progress) *View {
	v := &View{HasURL: s.URL != "", Title: s.Info.Title, Announce: s.Info.Announce, SupportURL: s.Info.SupportURL, WebPage: s.Info.WebPage,
		Used: s.Info.Upload + s.Info.Download, Total: s.Info.Total, Expire: s.Info.Expire, Updated: s.Updated, UpdateHours: s.Info.UpdateHours,
		Groups: subscription.Groups(s.Servers), Rules: map[string]string{}, Defaults: map[string]string{}, Custom: map[string]string{},
		Current: s.Current, NetKind: kind, Testing: pr.Testing, Tested: pr.Tested, TestTotal: pr.Total, TestStarted: pr.Started}
	if u, err := url.Parse(s.URL); err == nil {
		v.Host = u.Hostname()
	}
	for _, k := range subscription.NetKinds {
		v.Rules[k] = s.Rule(k)
		v.Defaults[k] = subscription.DefaultRule(k, s.Servers)
		if r := s.Rules[k]; r != "" {
			v.Custom[k] = r
		}
	}
	for _, srv := range s.Servers {
		sv := Server{ID: srv.ID, Name: srv.Name, Group: srv.Group, Proto: srv.Proto, Host: srv.Host, Port: srv.Port}
		if m, ok := s.Measures[srv.ID]; ok {
			sv.Ms, sv.Err, sv.At = m.Ms, i18n.T(m.Err), m.At
		}
		v.Servers = append(v.Servers, sv)
	}
	return v
}

// NetName names a kind of network for the journal.
func NetName(kind string) string {
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

// RuleName describes a rule for the journal.
func RuleName(rule string, servers []subscription.Server) string {
	if id := subscription.FixedID(rule); id != "" {
		if s := subscription.Find(servers, id); s != nil {
			return s.Name
		}
	}
	switch {
	case rule == subscription.RuleAuto:
		return "самый быстрый"
	case strings.HasPrefix(rule, "auto:!"):
		return fmt.Sprintf("самый быстрый, кроме %s", rule[len("auto:!"):])
	case strings.HasPrefix(rule, "auto:"):
		return fmt.Sprintf("самый быстрый из %s", rule[len("auto:"):])
	}
	return rule
}

// Alert is the warning about the end of the paid period or the traffic, or "" when all is well.
func Alert(in subscription.Info, warnBefore time.Duration) string {
	if in.Expire > 0 {
		until := time.Unix(in.Expire, 0)
		switch left := time.Until(until); {
		case left <= 0:
			return "Подписка закончилась — продлите её у провайдера"
		case left < warnBefore:
			return fmt.Sprintf("Подписка заканчивается %s", until.Local().Format("02.01.2006 в 15:04"))
		}
	}
	if in.Total > 0 && in.Upload+in.Download >= in.Total*9/10 {
		gb := func(n int64) string { return fmt.Sprintf("%.1f", float64(n)/(1<<30)) }
		return fmt.Sprintf("Израсходовано %s из %s ГБ трафика подписки", gb(in.Upload+in.Download), gb(in.Total))
	}
	return ""
}

// Name is the profile name for a new subscription.
func Name(info subscription.Info, link string, servers []subscription.Server) string {
	if info.Title != "" {
		return info.Title
	}
	if u, err := url.Parse(link); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	if len(servers) > 0 {
		return servers[0].Name
	}
	return ""
}

// IsLink reports whether text is a subscription URL or proxy server links.
func IsLink(text string) bool {
	low := strings.ToLower(strings.TrimSpace(text))
	for _, p := range []string{"https://", "http://", "vless://", "trojan://", "ss://", "vmess://"} {
		if strings.HasPrefix(low, p) {
			return true
		}
	}
	return false
}
