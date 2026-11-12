// Package rudomains is the "Russia past the VPN" list by domain: the Russian zones and the Russian
// services that live outside them. A site matches by its domain and every subdomain of it.
//
// Domains replace the RIPE list of Russian networks, which takes over twenty thousand routes on
// Windows: routes now appear only for the Russian sites actually opened (Windows, AmneziaWG), or
// none at all (Xray routes by domain itself).
package rudomains

import (
	"strings"

	"golang.org/x/net/idna"
)

// Default is the built-in list. Zones first: .ru, .su, .рф (xn--p1ai).
var Default = []string{
	"ru", "su", "xn--p1ai",
	// social networks, mail, search and their content servers
	"vk.com", "vk.me", "vk.ru", "userapi.com", "vkuser.net", "vkuseraudio.net", "vkuservideo.net", "vk-portal.net", "mycdn.me",
	"yandex.net", "yandex.com", "yastatic.net", "yandexcloud.net", "ya.ru",
	// marketplaces, classifieds and maps
	"wbstatic.net", "wbbasket.ru", "ozonusercontent.com", "avito.st", "2gis.com",
	// banks and payments
	"sberbank.com", "tbank-online.com", "moex.com", "nspk.ru",
	// video and streaming
	"ivi.tv", "okko.tv", "premier.one", "more.tv",
}

// Normalize turns user input ("Госуслуги.рф", "*.example.com", "https://site.ru/x") into a
// lowercase ASCII domain, or "" when it is not one.
func Normalize(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/:?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.Trim(strings.TrimPrefix(s, "*."), ".")
	if s == "" {
		return ""
	}
	a, err := idna.Lookup.ToASCII(s)
	if err != nil {
		return ""
	}
	return a
}

// List is the built-in list plus the user's extra domains, normalized and without repeats.
func List(extra []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range append(append([]string(nil), Default...), extra...) {
		if d = Normalize(d); d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

// Matcher tells whether a name belongs to the list.
type Matcher struct{ set map[string]bool }

func NewMatcher(list []string) *Matcher {
	m := &Matcher{set: make(map[string]bool, len(list))}
	for _, d := range list {
		m.set[d] = true
	}
	return m
}

// Match reports whether name or one of its parent domains is in the list.
func (m *Matcher) Match(name string) bool {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	for name != "" {
		if m.set[name] {
			return true
		}
		i := strings.IndexByte(name, '.')
		if i < 0 {
			return false
		}
		name = name[i+1:]
	}
	return false
}
