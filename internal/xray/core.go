package xray

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"

	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/infra/conf/serial"
	"github.com/xtls/xray-core/transport/internet"

	// Everything an outbound needs: dispatcher, proxies, transports, TLS and REALITY.
	_ "github.com/xtls/xray-core/app/dispatcher"
	_ "github.com/xtls/xray-core/app/policy"
	_ "github.com/xtls/xray-core/app/proxyman/inbound"
	_ "github.com/xtls/xray-core/app/proxyman/outbound"
	_ "github.com/xtls/xray-core/app/router"
	_ "github.com/xtls/xray-core/proxy/blackhole"
	_ "github.com/xtls/xray-core/proxy/freedom"
	_ "github.com/xtls/xray-core/proxy/shadowsocks"
	_ "github.com/xtls/xray-core/proxy/shadowsocks_2022"
	_ "github.com/xtls/xray-core/proxy/trojan"
	_ "github.com/xtls/xray-core/proxy/vless/outbound"
	_ "github.com/xtls/xray-core/proxy/vmess/outbound"
	_ "github.com/xtls/xray-core/transport/internet/grpc"
	_ "github.com/xtls/xray-core/transport/internet/httpupgrade"
	_ "github.com/xtls/xray-core/transport/internet/kcp"
	_ "github.com/xtls/xray-core/transport/internet/reality"
	_ "github.com/xtls/xray-core/transport/internet/splithttp"
	_ "github.com/xtls/xray-core/transport/internet/tcp"
	_ "github.com/xtls/xray-core/transport/internet/tls"
	_ "github.com/xtls/xray-core/transport/internet/udp"
	_ "github.com/xtls/xray-core/transport/internet/websocket"
)

// Instance is a running Xray core with one outbound: the server.
type Instance struct {
	core *core.Instance
	link *Link
}

// Start runs Xray for the server of link. address, when not empty, is the server's IP resolved in
// advance: the system resolver may be the one that goes through the tunnel.
func Start(link *Link, address string, direct ...string) (*Instance, error) {
	cfg := map[string]any{
		"log":       map[string]any{"loglevel": "none", "access": "none", "error": "none"},
		"outbounds": []any{link.Outbound("proxy", address)},
		"policy": map[string]any{"levels": map[string]any{"0": map[string]any{
			"handshake": 8, "connIdle": 300, "uplinkOnly": 1, "downlinkOnly": 1,
		}}},
	}
	// direct: domains (and their subdomains) that go straight out, not through the server — the
	// "Russia past the VPN" list. The engine sniffs the site name from TLS, HTTP and QUIC for it.
	if len(direct) > 0 {
		doms := make([]string, len(direct))
		for i, d := range direct {
			doms[i] = "domain:" + d
		}
		cfg["outbounds"] = []any{link.Outbound("proxy", address), map[string]any{"tag": "direct", "protocol": "freedom"}}
		cfg["routing"] = map[string]any{"domainStrategy": "AsIs", "rules": []any{
			map[string]any{"type": "field", "domain": doms, "outboundTag": "direct"},
		}}
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	jc, err := serial.DecodeJSONConfig(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("Xray не принял настройки сервера: %w", err)
	}
	pb, err := jc.Build()
	if err != nil {
		return nil, fmt.Errorf("Xray не принял настройки сервера: %w", err)
	}
	inst, err := core.New(pb)
	if err != nil {
		return nil, fmt.Errorf("Xray не запустился: %w", err)
	}
	if err := inst.Start(); err != nil {
		inst.Close()
		return nil, fmt.Errorf("Xray не запустился: %w", err)
	}
	return &Instance{core: inst, link: link}, nil
}

// Close stops the core and its connections.
func (i *Instance) Close() { i.core.Close() }

// Dial opens a TCP connection to addr ("host:port") through the server.
func (i *Instance) Dial(ctx context.Context, addr string) (net.Conn, error) {
	dest, err := xnet.ParseDestination("tcp:" + addr)
	if err != nil {
		return nil, err
	}
	return core.Dial(ctx, i.core, dest)
}

// ProbeURL is fetched through a server to measure it: a tiny response that works everywhere.
const ProbeURL = "http://cp.cloudflare.com/generate_204"

// Delay fetches ProbeURL through the server and returns how long the whole request took (the
// "real delay" of v2rayN and Happ: connection, handshake with the server and the answer).
func (i *Instance) Delay(ctx context.Context) (time.Duration, error) {
	start := time.Now()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ProbeURL, nil)
	host := req.URL.Host
	if !strings.Contains(host, ":") {
		host += ":80"
	}
	c, err := i.Dial(ctx, host)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Close = true
	if err := req.Write(c); err != nil {
		return 0, errorFor(ctx, err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), req)
	if err != nil {
		return 0, errorFor(ctx, err)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return 0, fmt.Errorf("сервер ответил %s", resp.Status)
	}
	return time.Since(start), nil
}

func errorFor(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return errors.New("нет ответа")
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return errors.New("сервер закрыл соединение")
	}
	return err
}

// ---- sockets of the core ------------------------------------------------------------------

// SocketHook, when set, prepares every socket that Xray opens to a server, before it connects:
// on Windows it pins the socket to the physical interface so that it cannot loop back into the
// tunnel. network is "tcp4", "tcp6", "udp4" or "udp6".
type SocketHook func(network string, fd uintptr) error

var (
	hookMu   sync.RWMutex
	hook     SocketHook
	hookOnce sync.Once
)

// SetSocketHook installs (or with nil removes) the hook for sockets opened from now on.
func SetSocketHook(h SocketHook) {
	hookOnce.Do(func() {
		internet.RegisterDialerController(func(network, address string, c syscall.RawConn) error {
			hookMu.RLock()
			h := hook
			hookMu.RUnlock()
			if h == nil {
				return nil
			}
			if ap, err := netipAddrPort(address); err == nil && ap.IsLoopback() {
				return nil
			}
			network = family(network, address)
			var herr error
			if err := c.Control(func(fd uintptr) { herr = h(network, fd) }); err != nil {
				return err
			}
			return herr
		})
	})
	hookMu.Lock()
	hook = h
	hookMu.Unlock()
}

func netipAddrPort(address string) (net.IP, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, errors.New("not an IP")
	}
	return ip, nil
}

// family completes "tcp"/"udp" with the address family of the destination.
func family(network, address string) string {
	if strings.HasSuffix(network, "4") || strings.HasSuffix(network, "6") {
		return network
	}
	if ip, err := netipAddrPort(address); err == nil && ip.To4() == nil {
		return network + "6"
	}
	return network + "4"
}
