package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sort"
	"strings"
	"time"

	"awgpilot/internal/ipc"
	"awgpilot/internal/subscription"
	"awgpilot/internal/xray"
)

// proxyCheck drives the installed AWG Pilot service through its pipe, like the window does: it
// checks the servers of a subscription, brings the tunnel up through the fastest one, verifies
// that traffic, DNS and the in-tunnel ping go through it, switches to another server and goes
// down. Needs the service, not administrator rights. Links are never printed.
//
//	livecheck proxy -links <file with share links, or a decoded subscription> [-kill]
func proxyCheck(args []string) {
	fs := flag.NewFlagSet("proxy", flag.ExitOnError)
	linksFile := fs.String("links", "", "file with vless://… links, one per line")
	kill := fs.Bool("kill", false, "turn the kill switch on while connected")
	fs.Parse(args)
	data, err := os.ReadFile(*linksFile)
	if err != nil {
		fmt.Println("нужен -links <файл>")
		os.Exit(2)
	}
	servers, skipped := subscription.Parse(data)
	check("разбор подписки", len(servers) > 0, fmt.Sprintf("серверов %d, пропущено %d, группы %s", len(servers), len(skipped), strings.Join(subscription.Groups(servers), ", ")))
	if len(servers) == 0 {
		return
	}

	var c ipc.Client
	ver, err := c.Version()
	check("служба отвечает", err == nil, errOr(err, "версия "+ver))
	if err != nil {
		return
	}
	before, _ := trace()

	// 1. server check through the service, in parts as the window sends them
	start := time.Now()
	type res struct {
		s  subscription.Server
		ms int64
	}
	var all []res
	for i := 0; i < len(servers); i += 16 {
		part := servers[i:min(i+16, len(servers))]
		links := make([]string, len(part))
		for j, s := range part {
			links[j] = s.Link
		}
		r, err := c.TestServers(links)
		if err != nil {
			check("проверка серверов службой", false, err.Error())
			return
		}
		for j := range r {
			all = append(all, res{part[j], r[j].Ms})
		}
	}
	var working []res
	for _, r := range all {
		if r.ms > 0 {
			working = append(working, r)
		}
	}
	sort.Slice(working, func(i, j int) bool { return working[i].ms < working[j].ms })
	check("проверка серверов службой", len(working) > 0, fmt.Sprintf("отвечают %d из %d за %s", len(working), len(all), time.Since(start).Round(time.Second)))
	if len(working) == 0 {
		return
	}
	for i, w := range working[:min(5, len(working))] {
		logf("      %d. %s — %d мс", i+1, w.s.Name, w.ms)
	}

	up := func(s subscription.Server) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		addr, err := xray.Resolve(ctx, s.Host)
		cancel()
		if err != nil {
			check("адрес сервера", false, err.Error())
			return false
		}
		t0 := time.Now()
		err = c.Up(ipc.UpParams{Proxy: s.Link, ProxyAddr: addr, FullTunnel: true, KillSwitch: *kill, BlockIPv6: true,
			DNS: []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("1.0.0.1")}})
		check("туннель через «"+s.Name+"»", err == nil, errOr(err, fmt.Sprintf("поднят за %s", time.Since(t0).Round(time.Millisecond))))
		if err != nil {
			return false
		}
		deadline := time.Now().Add(25 * time.Second)
		for time.Now().Before(deadline) {
			st := c.Stats()
			if st.Up && !st.LastHandshake.IsZero() && st.PingMs > 0 {
				check("пинг через сервер", true, fmt.Sprintf("%d мс, сеть %q, сервер %s", st.PingMs, st.NetKind, st.ServerName))
				return true
			}
			time.Sleep(500 * time.Millisecond)
		}
		st := c.Stats()
		check("пинг через сервер", false, fmt.Sprintf("нет ответа за 25 с (up=%v, неудач %d)", st.Up, st.PingFails))
		return false
	}

	defer func() {
		c.Down(true)
		time.Sleep(2 * time.Second)
		after, err := trace()
		check("после отключения", !c.Stats().Up, errOr(err, "выход снова "+after.String()))
	}()

	if !up(working[0].s) {
		// diagnostics: does a fresh check of the same server work with the tunnel up?
		r, err := c.TestServers([]string{working[0].s.Link})
		if err == nil {
			logf("      проверка того же сервера при поднятом туннеле: %d мс %s", r[0].Ms, r[0].Error)
		}
	}
	time.Sleep(4 * time.Second) // Windows activates the adapter's routes and DNS
	via, err := trace()
	check("трафик идёт через сервер", err == nil && via.ip != before.ip, errOr(err, fmt.Sprintf("выход %s (без VPN был %s)", via, before)))
	_, err = net.DefaultResolver.LookupHost(context.Background(), "example.com")
	check("DNS через туннель", err == nil, errOr(err, "example.com разрешился"))
	mbps, err := download()
	check("загрузка 10 МБ", err == nil, errOr(err, fmt.Sprintf("%.1f Мбит/с", mbps)))
	st := c.Stats()
	check("счётчики", st.RxBytes > 1e6 && st.TxBytes > 0, fmt.Sprintf("↓ %d КБ, ↑ %d КБ, IPv6 закрыт: %v, kill switch: %v", st.RxBytes>>10, st.TxBytes>>10, st.IPv6Blocked, st.KillSwitch))

	// 2. move to another server, as the watchdog does when one dies
	if len(working) > 1 {
		c.Down(false)
		if up(working[1].s) {
			time.Sleep(4 * time.Second)
			v2, err := trace()
			check("второй сервер", err == nil, errOr(err, "выход "+v2.String()))
		}
	}
}

type exit struct{ ip, loc string }

func (e exit) String() string { return mask(e.ip) + " (" + e.loc + ")" }

// trace asks Cloudflare which address and country the request came from.
func trace() (exit, error) {
	cl := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	var e exit
	resp, err := cl.Get("https://www.cloudflare.com/cdn-cgi/trace")
	if err != nil {
		return e, err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), "=")
		switch k {
		case "ip":
			e.ip = v
		case "loc":
			e.loc = v
		}
	}
	return e, nil
}

func download() (float64, error) {
	cl := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	t0 := time.Now()
	resp, err := cl.Get("https://speed.cloudflare.com/__down?bytes=10000000")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		return 0, err
	}
	return float64(n) * 8 / 1e6 / time.Since(t0).Seconds(), nil
}
