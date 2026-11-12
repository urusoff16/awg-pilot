package xray

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"time"

	"awgpilot/internal/netx"
)

// Result is a measurement of one server.
type Result struct {
	Ms    int64  `json:"ms"`    // real delay in milliseconds; -1 = failed
	Error string `json:"error"` // why it failed
}

// Resolve returns an IPv4 address of host (IPv6 if it has none), or host itself if it already is
// an address. Server names are resolved before a tunnel comes up, because once it is up the
// system resolver may go through the tunnel.
func Resolve(ctx context.Context, host string) (string, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Unmap().String(), nil
	}
	ips, err := netx.Resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return "", err
	}
	for _, ip := range ips {
		if ip.Unmap().Is4() {
			return ip.Unmap().String(), nil
		}
	}
	if len(ips) > 0 {
		return ips[0].String(), nil
	}
	return "", &net.DNSError{Err: "no address", Name: host, IsNotFound: true}
}

// Test measures one server: resolve, start a core, fetch ProbeURL through it. A failed first try
// gets a second one, because on lossy mobile networks one lost handshake is common.
func Test(ctx context.Context, link string, timeout time.Duration) Result {
	l, err := ParseLink(link)
	if err != nil {
		return Result{Ms: -1, Error: err.Error()}
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	addr, err := Resolve(rctx, l.Host)
	cancel()
	if err != nil {
		return Result{Ms: -1, Error: "адрес сервера не найден"}
	}
	inst, err := Start(l, addr)
	if err != nil {
		return Result{Ms: -1, Error: err.Error()}
	}
	defer inst.Close()
	var last error
	for try := 0; try < 2 && ctx.Err() == nil; try++ {
		tctx, cancel := context.WithTimeout(ctx, timeout)
		d, err := inst.Delay(tctx)
		cancel()
		if err == nil {
			return Result{Ms: max(d.Milliseconds(), 1)}
		}
		last = err
	}
	if last == nil {
		last = ctx.Err()
	}
	return Result{Ms: -1, Error: last.Error()}
}

// TestAll measures links in parallel (at most parallel at a time) and returns results in the
// same order. done, if set, is called as each result arrives.
func TestAll(ctx context.Context, links []string, parallel int, timeout time.Duration, done func(i int, r Result)) []Result {
	if parallel < 1 {
		parallel = 1
	}
	out := make([]Result, len(links))
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, l := range links {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				out[i] = Result{Ms: -1, Error: "проверка прервана"}
				return
			}
			defer func() { <-sem }()
			out[i] = Test(ctx, l, timeout)
			if done != nil {
				done(i, out[i])
			}
		}()
	}
	wg.Wait()
	return out
}
