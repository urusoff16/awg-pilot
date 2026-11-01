// Package geoip provides the list of Russian IPv4 networks from RIPE NCC (the regional
// registry that allocates them), cached on disk and refreshed weekly.
package geoip

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	"awgpilot/internal/netx"
)

const (
	RipeURL = "https://stat.ripe.net/data/country-resource-list/data.json?resource=RU&v4_format=prefix"
	MaxAge  = 7 * 24 * time.Hour
	// minPrefixes guards against caching a truncated or bogus answer (RU has ~11 000 networks).
	minPrefixes = 1000
)

type List struct {
	Prefixes  []netip.Prefix
	FetchedAt time.Time
}

// Fetch downloads the current list from RIPEstat.
func Fetch(ctx context.Context, url string) (*List, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := netx.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("RIPE NCC недоступен: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("RIPE NCC ответил HTTP %d", resp.StatusCode)
	}
	var body struct {
		Status string `json:"status"`
		Data   struct {
			Resources struct {
				IPv4 []string `json:"ipv4"`
			} `json:"resources"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("не удалось разобрать ответ RIPE NCC: %w", err)
	}
	l := &List{FetchedAt: time.Now()}
	for _, s := range body.Data.Resources.IPv4 {
		if p, err := netip.ParsePrefix(strings.TrimSpace(s)); err == nil && p.Addr().Is4() {
			l.Prefixes = append(l.Prefixes, p.Masked())
		}
	}
	if len(l.Prefixes) < minPrefixes {
		return nil, fmt.Errorf("RIPE NCC вернул подозрительно короткий список (%d сетей)", len(l.Prefixes))
	}
	return l, nil
}

// Load reads a cached list.
func Load(path string) (*List, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	l := &List{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if ts, ok := strings.CutPrefix(line, "# fetched "); ok {
			l.FetchedAt, _ = time.Parse(time.RFC3339, ts)
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if p, err := netip.ParsePrefix(line); err == nil && p.Addr().Is4() {
			l.Prefixes = append(l.Prefixes, p)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(l.Prefixes) < minPrefixes {
		return nil, errors.New("кэш списка российских сетей повреждён")
	}
	return l, nil
}

// Save writes the list atomically.
func (l *List) Save(path string) error {
	var b strings.Builder
	b.WriteString("# Russian IPv4 networks, RIPE NCC country-resource-list\n")
	b.WriteString("# fetched " + l.FetchedAt.UTC().Format(time.RFC3339) + "\n")
	for _, p := range l.Prefixes {
		b.WriteString(p.String())
		b.WriteByte('\n')
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Get returns the cached list, refreshing it when older than MaxAge. If RIPE NCC cannot be
// reached a stale cache is still returned together with a warning error.
func Get(ctx context.Context, cachePath string) (*List, error) {
	cached, cerr := Load(cachePath)
	if cerr == nil && time.Since(cached.FetchedAt) < MaxAge {
		return cached, nil
	}
	fresh, ferr := Fetch(ctx, RipeURL)
	if ferr == nil {
		if err := fresh.Save(cachePath); err != nil {
			return fresh, fmt.Errorf("список получен, но не сохранён: %w", err)
		}
		return fresh, nil
	}
	if cerr == nil {
		return cached, fmt.Errorf("используется список от %s: %w", cached.FetchedAt.Local().Format("02.01.2006"), ferr)
	}
	return nil, ferr
}
