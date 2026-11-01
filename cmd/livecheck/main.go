// livecheck runs end-to-end checks against the real provider and a real tunnel.
// It never prints keys or private config values; IP addresses are partially masked.
//
//	livecheck api      -key-file <file with vpn:// key> [-download]   (uses AWG Pilot's own device ID)
//	livecheck tunnel   -conf <AmneziaWG .conf> [-log <file>]      (needs administrator rights)
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/amnezia-vpn/amneziawg-windows/conf"

	"awgpilot/internal/keyapi"
	"awgpilot/internal/remotecfg"
	"awgpilot/internal/store"
	"awgpilot/internal/tunnel"
	"awgpilot/internal/vpnkey"
	"awgpilot/internal/wfp"
)

var out io.Writer = os.Stdout
var failures int

func logf(format string, a ...any) {
	fmt.Fprintf(out, time.Now().Format("15:04:05 ")+format+"\n", a...)
}

func check(name string, ok bool, detail string) {
	status := "PASS"
	if !ok {
		status = "FAIL"
		failures++
	}
	logf("%s  %s — %s", status, name, detail)
}

func mask(ip string) string {
	if a, err := netip.ParseAddr(ip); err == nil && a.Is4() {
		b := a.As4()
		return fmt.Sprintf("%d.x.x.%d", b[0], b[3])
	}
	if len(ip) > 8 {
		return ip[:6] + "…"
	}
	return ip
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: livecheck api|tunnel ...")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "api":
		apiCheck(os.Args[2:])
	case "tunnel":
		tunnelCheck(os.Args[2:])
	default:
		fmt.Println("unknown command")
		os.Exit(2)
	}
	logf("итог: %d провал(ов)", failures)
	if failures > 0 {
		os.Exit(1)
	}
}

// ---- API ----------------------------------------------------------------------------------

func apiCheck(args []string) {
	fs := flag.NewFlagSet("api", flag.ExitOnError)
	keyFile := fs.String("key-file", "", "file with the vpn:// key")
	download := fs.Bool("download", false, "also call download-awg-key (same device, no rebind)")
	fs.Parse(args)

	raw, err := os.ReadFile(*keyFile)
	if err != nil {
		logf("не удалось прочитать ключ: %v", err)
		os.Exit(2)
	}
	k, err := vpnkey.Decode(string(raw))
	check("ключ vpn://", err == nil, fmt.Sprintf("%v", errOr(err, k.APIEndpoint+" "+k.Masked())))
	if err != nil {
		return
	}
	var dev string
	if st, err := store.Open(); err == nil {
		if settings, err := st.LoadSettings(); err == nil {
			dev = settings.DeviceID
		}
	}
	check("ID устройства AWG Pilot", dev != "", map[bool]string{true: "взят из настроек AWG Pilot", false: "не найден — один раз запустите AWG Pilot"}[dev != ""])
	if dev == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rc, err := remotecfg.Fetch(ctx, remotecfg.GatewayConfigURL)
	check("настройки провайдера", err == nil, errOr(err, fmt.Sprintf("api=%s, типов подключения %d", safe(rc, func(c *remotecfg.Config) string { return c.APIBaseURL }), safeN(rc))))

	c := keyapi.New(k.APIEndpoint, dev, "AWGPilot/0.5.0 (Windows)")
	req, err := c.GetRequest(ctx, k.APIKey)
	if err == nil {
		check("get-request", true, fmt.Sprintf("статус=%s, оплачено до %s, устройств свободно %v из %v", req.SimplifiedStatus, req.PaidUntil, req.NumberOfAvailableKeys, req.KeyLimit))
	} else {
		check("get-request", false, err.Error())
	}
	cs, err := c.GetCountries(ctx)
	if err == nil {
		var names []string
		for _, x := range cs {
			names = append(names, x.ISOCountryCode)
		}
		if len(cs) == 0 {
			check("список стран", true, "провайдер не предлагает выбор страны (404) — используется его сервер по умолчанию")
		} else {
			check("список стран", true, fmt.Sprintf("%d: %s", len(cs), strings.Join(names, " ")))
		}
	} else {
		check("список стран", false, err.Error())
	}

	if !*download {
		logf("download-awg-key пропущен (нужен флаг -download)")
		return
	}
	opts := keyapi.DownloadOptions{}
	if rc != nil {
		if conn := rc.Connection(""); conn != nil {
			opts.RelativePath = conn.RelativePath
		}
	}
	cfgText, err := c.DownloadAWGKey(ctx, k.APIKey, opts)
	if err != nil {
		check("download-awg-key", false, err.Error())
		return
	}
	cfg, perr := tunnel.ParseConfig(cfgText)
	check("download-awg-key", perr == nil, errOr(perr, fmt.Sprintf("конфиг получен: сервер %s, адрес %v", maskEndpoint(cfg), addrs(cfg))))
	if dst := os.Getenv("LIVECHECK_SAVE_CONF"); dst != "" && perr == nil {
		os.WriteFile(dst, []byte(cfgText), 0o600)
		logf("конфиг сохранён в %s", dst)
	}
}

// ---- Tunnel -------------------------------------------------------------------------------

func tunnelCheck(args []string) {
	fs := flag.NewFlagSet("tunnel", flag.ExitOnError)
	confPath := fs.String("conf", "", "AmneziaWG .conf")
	logPath := fs.String("log", "", "write results to this file")
	fs.Parse(args)
	if *logPath != "" {
		f, err := os.Create(*logPath)
		if err == nil {
			defer f.Close()
			out = io.MultiWriter(os.Stdout, f)
		}
	}
	raw, err := os.ReadFile(*confPath)
	if err != nil {
		logf("не удалось прочитать конфиг: %v", err)
		failures++
		return
	}

	ipBefore, err := publicIP("https://api.ipify.org")
	check("IP до подключения", err == nil, errOr(err, mask(ipBefore)))
	v6Before, v6err := publicIP("https://api6.ipify.org")
	logf("IPv6 до подключения: %s", map[bool]string{true: "есть (" + mask(v6Before) + ")", false: "нет"}[v6err == nil])

	var m tunnel.Manager
	defer m.Down()

	// 1. full tunnel, IPv6 blocked, no kill switch
	err = m.Up(tunnel.Options{RawConfig: string(raw), AllowedIPs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}, FullTunnel: true, BlockIPv6: true, Logf: logf})
	check("подъём туннеля", err == nil, errOr(err, "адаптер AWGPilot создан"))
	if err != nil {
		return
	}
	hs := waitHandshake(&m, 20*time.Second)
	check("рукопожатие с сервером", hs, map[bool]string{true: "есть", false: "нет ответа за 20 с"}[hs])
	var ipVPN string
	n, err := retry(15, func() error {
		var e error
		ipVPN, e = publicIP("https://api.ipify.org")
		return e
	})
	check("IP через VPN", err == nil && ipVPN != ipBefore, errOr(err, fmt.Sprintf("%s (до: %s), попытка %d", mask(ipVPN), mask(ipBefore), n)))
	var addrs []string
	n, err = retry(15, func() error {
		var e error
		addrs, e = net.DefaultResolver.LookupHost(context.Background(), "example.com")
		return e
	})
	check("DNS через VPN", err == nil && len(addrs) > 0, errOr(err, fmt.Sprintf("example.com → %d адрес(ов), попытка %d", len(addrs), n)))
	if failures > 0 {
		diagnostics()
	}
	_, v6err = publicIP("https://api6.ipify.org")
	check("утечка IPv6 закрыта", v6err != nil, map[bool]string{true: "IPv6-запрос не прошёл", false: "IPv6 прошёл мимо VPN!"}[v6err != nil])
	st := m.Stats()
	logf("трафик: получено %d Б, отправлено %d Б, сервер %s", st.RxBytes, st.TxBytes, mask(strings.Split(st.Endpoint, ":")[0]))

	// 2. per-app rule while connected: curl must still work through the tunnel
	curl := filepath.Join(os.Getenv("SystemRoot"), "System32", "curl.exe")
	var rules wfp.AppRules
	err = rules.Apply([]string{curl}, m.LUID())
	check("правило «curl только через VPN»", err == nil, errOr(err, "установлено"))
	n, err = retry(5, func() error { return curlErr(curl) })
	check("curl через туннель при правиле", err == nil, errOr(err, fmt.Sprintf("доступ к 1.1.1.1, попытка %d", n)))

	m.Down()
	err = rules.Apply([]string{curl}, 0)
	check("правило при выключенном VPN", err == nil, errOr(err, "переустановлено"))
	check("curl без VPN заблокирован", !curlOK(curl), "сеть недоступна, как и задумано")
	rules.Close()
	check("curl после снятия правил", curlOK(curl), "сеть вернулась")

	// 3. kill switch
	err = m.Up(tunnel.Options{RawConfig: string(raw), AllowedIPs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}, FullTunnel: true, KillSwitch: true, BlockIPv6: true, Logf: logf})
	check("подъём с kill switch", err == nil, errOr(err, "WFP-фильтры установлены"))
	if err == nil {
		waitHandshake(&m, 20*time.Second)
		// curl is a separate process: the kill switch must let it out only through the tunnel.
		n, err := retry(10, func() error { return curlErr(curl) })
		check("интернет через туннель при kill switch", err == nil, errOr(err, fmt.Sprintf("curl → 1.1.1.1, попытка %d", n)))
		var ip string
		_, err = retry(10, func() error {
			out, e := exec.Command(curl, "-s", "--max-time", "8", "https://api.ipify.org").Output()
			ip = strings.TrimSpace(string(out))
			return e
		})
		check("внешний IP другой программы при kill switch", err == nil && ip == ipVPN, errOr(err, mask(ip)+" (ожидался IP VPN)"))
		m.Down()
	}

	ipAfter, err := publicIP("https://api.ipify.org")
	check("сеть после отключения", err == nil && ipAfter == ipBefore, errOr(err, mask(ipAfter)))
}

func waitHandshake(m *tunnel.Manager, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !m.Stats().LastHandshake.IsZero() {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

func publicIP(url string) (string, error) {
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 100))
	return strings.TrimSpace(string(b)), err
}

func curlOK(curl string) bool { return curlErr(curl) == nil }

func curlErr(curl string) error {
	return exec.Command(curl, "-s", "-o", "NUL", "--max-time", "8", "https://1.1.1.1").Run()
}

// retry runs f up to n times one second apart and returns the attempt number that succeeded.
func retry(n int, f func() error) (int, error) {
	var err error
	for i := 1; i <= n; i++ {
		if err = f(); err == nil {
			return i, nil
		}
		time.Sleep(time.Second)
	}
	return n, err
}

// diagnostics logs routing and DNS state when connectivity checks fail.
func diagnostics() {
	logf("---- диагностика ----")
	for _, args := range [][]string{
		{"route", "print", "-4", "0.0.0.0"},
		{"netsh", "interface", "ipv4", "show", "interfaces"},
		{"netsh", "interface", "ipv4", "show", "dnsservers", "name=AWGPilot"},
		{"nslookup", "example.com", "172.27.0.1"},
		{"nslookup", "example.com", "8.8.8.8"},
	} {
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		logf("$ %s (err=%v)\n%s", strings.Join(args, " "), err, decodeOEM(out))
	}
	logf("---- конец диагностики ----")
}

// decodeOEM keeps console output readable: non-ASCII OEM bytes are replaced, ASCII kept.
func decodeOEM(b []byte) string {
	s := make([]rune, 0, len(b))
	for _, c := range b {
		if c < 128 {
			s = append(s, rune(c))
		} else {
			s = append(s, '?')
		}
	}
	return string(s)
}

func errOr(err error, ok string) string {
	if err != nil {
		return err.Error()
	}
	return ok
}

func safe(c *remotecfg.Config, f func(*remotecfg.Config) string) string {
	if c == nil {
		return "—"
	}
	return f(c)
}

func safeN(c *remotecfg.Config) int {
	if c == nil {
		return 0
	}
	return len(c.Connections)
}

func maskEndpoint(c *conf.Config) string {
	if c == nil || len(c.Peers) == 0 {
		return "—"
	}
	return mask(c.Peers[0].Endpoint.Host) + fmt.Sprintf(":%d", c.Peers[0].Endpoint.Port)
}

func addrs(c *conf.Config) string {
	if c == nil {
		return "—"
	}
	var s []string
	for _, a := range c.Interface.Addresses {
		s = append(s, a.String())
	}
	return strings.Join(s, ",")
}
