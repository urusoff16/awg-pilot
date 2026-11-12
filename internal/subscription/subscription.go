// Package subscription downloads proxy subscriptions (the format of Remnawave, Marzban, 3x-ui and
// others: a list of share links, usually in base64, with the account in HTTP headers), keeps
// their servers and picks the server to use for the current kind of network.
package subscription

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"awgpilot/internal/xray"
)

// Server is one server of a subscription.
type Server struct {
	ID    string `json:"id"`    // stable across updates while the server stays the same
	Name  string `json:"name"`  // as the provider calls it, with the flag
	Group string `json:"group"` // "LTE", "SMART", "BRIDGE"... from the name; "" if none
	Proto string `json:"proto"` // vless, trojan, ss, vmess
	Host  string `json:"host"`
	Port  int    `json:"port"`
	Link  string `json:"link"` // the share link (secret: it holds the user's credentials)
}

// Info is what the provider tells about the subscription in the response headers.
type Info struct {
	Title       string `json:"title"`
	Announce    string `json:"announce"`
	SupportURL  string `json:"support_url"`
	WebPage     string `json:"web_page"`
	Upload      int64  `json:"upload"`
	Download    int64  `json:"download"`
	Total       int64  `json:"total"`        // 0 = unlimited
	Expire      int64  `json:"expire"`       // unix seconds; 0 = never
	UpdateHours int    `json:"update_hours"` // how often the provider wants the list refreshed
}

// Headers identify this device to the provider. Panels with a device limit (HWID) count devices
// by HWID; it is AWG Pilot's own random ID, nothing is read from other programs.
type Headers struct {
	UserAgent string
	HWID      string
	OS        string // "Windows", "Android"
	OSVersion string
	Model     string
}

// Fetch downloads and parses a subscription.
func Fetch(ctx context.Context, client *http.Client, link string, h Headers) (Info, []Server, error) {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return Info{}, nil, errors.New("ссылка на подписку должна начинаться с https://")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Info{}, nil, err
	}
	req.Header.Set("User-Agent", h.UserAgent)
	req.Header.Set("Accept", "*/*")
	if h.HWID != "" {
		req.Header.Set("x-hwid", h.HWID)
	}
	if h.OS != "" {
		req.Header.Set("x-device-os", h.OS)
	}
	if h.OSVersion != "" {
		req.Header.Set("x-ver-os", h.OSVersion)
	}
	if h.Model != "" {
		req.Header.Set("x-device-model", h.Model)
	}
	resp, err := client.Do(req)
	if err != nil {
		return Info{}, nil, fmt.Errorf("подписка не загрузилась: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return Info{}, nil, fmt.Errorf("подписка не загрузилась: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 200 || !utf8.ValidString(msg) || strings.Contains(msg, "<") {
			msg = ""
		}
		if msg != "" {
			return Info{}, nil, fmt.Errorf("провайдер ответил %s: %s", resp.Status, msg)
		}
		return Info{}, nil, fmt.Errorf("провайдер ответил %s", resp.Status)
	}
	info := ParseHeaders(resp.Header)
	servers, skipped := Parse(body)
	if len(servers) == 0 {
		if strings.Contains(strings.ToLower(string(body[:min(len(body), 512)])), "<html") {
			return info, nil, errors.New("по ссылке открылась веб-страница, а не подписка — скопируйте ссылку на подписку для приложений")
		}
		if len(skipped) > 0 {
			return info, nil, fmt.Errorf("в подписке нет серверов, которые поддерживает AWG Pilot (%s)", skipped[0])
		}
		return info, nil, errors.New("в подписке нет серверов")
	}
	return info, servers, nil
}

// ParseHeaders reads the subscription headers (values may be "base64:..." for non-ASCII text).
func ParseHeaders(h http.Header) Info {
	var in Info
	in.Title = headerText(h.Get("profile-title"))
	in.Announce = headerText(h.Get("announce"))
	in.SupportURL = webURL(h.Get("support-url"))
	in.WebPage = webURL(h.Get("profile-web-page-url"))
	if v, err := strconv.ParseFloat(strings.TrimSpace(h.Get("profile-update-interval")), 64); err == nil && v > 0 {
		in.UpdateHours = max(int(v), 1)
	}
	for _, part := range strings.Split(h.Get("subscription-userinfo"), ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			continue
		}
		switch strings.ToLower(k) {
		case "upload":
			in.Upload = int64(n)
		case "download":
			in.Download = int64(n)
		case "total":
			in.Total = int64(n)
		case "expire":
			in.Expire = int64(n)
		}
	}
	return in
}

func headerText(v string) string {
	v = strings.TrimSpace(v)
	if rest, ok := strings.CutPrefix(v, "base64:"); ok {
		if b, err := decodeB64(rest); err == nil && utf8.Valid(b) {
			return strings.TrimSpace(string(b))
		}
		return ""
	}
	if !utf8.ValidString(v) {
		return ""
	}
	return v
}

// webURL keeps only http(s) and Telegram links: the UI opens them on click.
func webURL(v string) string {
	v = strings.TrimSpace(v)
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		return ""
	}
	switch u.Scheme {
	case "https", "http", "tg":
		return v
	}
	return ""
}

func decodeB64(s string) ([]byte, error) {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
	s = strings.TrimRight(s, "=")
	for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("не base64")
}

// Parse reads a subscription body: share links one per line, plain or in base64. skipped lists
// why lines were left out (unsupported protocols, broken links).
func Parse(body []byte) (servers []Server, skipped []string) {
	text := strings.TrimSpace(string(body))
	if !strings.Contains(text, "://") {
		if b, err := decodeB64(text); err == nil {
			text = string(b)
		}
	}
	seen := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		l, err := xray.ParseLink(line)
		if err != nil {
			skipped = append(skipped, err.Error())
			continue
		}
		s := Server{Name: l.Name, Proto: l.Proto, Host: l.Host, Port: l.Port, Link: line}
		if s.Name == "" {
			s.Name = l.Endpoint()
		}
		s.Group = GroupOf(s.Name)
		s.ID = linkID(line)
		for seen[s.ID] { // the same server twice under different names
			s.ID += "+"
		}
		seen[s.ID] = true
		servers = append(servers, s)
	}
	return servers, skipped
}

// linkID identifies a server by its link without the name, so renaming keeps measurements.
func linkID(link string) string {
	if i := strings.IndexByte(link, '#'); i >= 0 && !strings.HasPrefix(link, "vmess://") {
		link = link[:i]
	}
	sum := sha256.Sum256([]byte(link))
	return hex.EncodeToString(sum[:6])
}

// homoglyphs maps Cyrillic letters that look Latin ("LTЕ", "SМART" in real subscriptions).
var homoglyphs = strings.NewReplacer("А", "A", "В", "B", "Е", "E", "К", "K", "М", "M", "Н", "H", "О", "O", "Р", "P", "С", "C", "Т", "T", "Х", "X", "У", "Y")

// knownGroups are words providers use to mark kinds of servers.
var knownGroups = []string{"LTE", "SMART", "BRIDGE", "WIFI", "WI-FI", "MOBILE", "YOUTUBE", "GAMING", "TORRENT"}

// GroupOf derives the group of a server from its name: a known word ("LTE"), or the prefix before
// a dash ("SMART-Нидерланды" → "SMART").
func GroupOf(name string) string {
	up := strings.ToUpper(homoglyphs.Replace(name))
	for _, g := range knownGroups {
		if containsWord(up, g) {
			if g == "WI-FI" {
				return "WIFI"
			}
			return g
		}
	}
	// "🇳🇱 SMART-Нидерланды": the Latin word before the dash
	fields := strings.Fields(up)
	for _, f := range fields {
		pre, _, ok := strings.Cut(f, "-")
		if ok && len(pre) >= 2 && isLatinWord(pre) {
			return pre
		}
	}
	return ""
}

func isLatinWord(s string) bool {
	for _, r := range s {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func containsWord(s, w string) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], w)
		if j < 0 {
			return false
		}
		j += i
		before, after := j == 0, j+len(w) == len(s)
		if !before {
			r, _ := utf8.DecodeLastRuneInString(s[:j])
			before = !isWordRune(r)
		}
		if !after {
			r, _ := utf8.DecodeRuneInString(s[j+len(w):])
			after = !isWordRune(r)
		}
		if before && after {
			return true
		}
		i = j + 1
	}
}

func isWordRune(r rune) bool {
	return r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
}

// Expired reports whether the subscription has ended.
func (in Info) Expired(now time.Time) bool { return in.Expire > 0 && now.Unix() >= in.Expire }
