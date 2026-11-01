// Package speedtest measures download and upload throughput against Cloudflare's speed test
// endpoints (speed.cloudflare.com), the same service the cloudflare speed test page uses.
// The caller supplies the dialer, so the test can be pinned to the VPN tunnel.
package speedtest

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	host       = "speed.cloudflare.com"
	streams    = 4               // parallel connections, like browser speed tests
	phaseTime  = 8 * time.Second // per direction
	warmup     = time.Second     // TCP slow start is left out of the result
	downChunk  = 5_000_000       // bytes per request: from VPN addresses Cloudflare refuses 10 MB and more
	upChunk    = 5_000_000       // bytes sent per upload request
	tickPeriod = 250 * time.Millisecond
)

// Dialer opens a TCP connection to addr ("host:port").
type Dialer func(ctx context.Context, network, addr string) (net.Conn, error)

// Progress reports the phase ("connected", "download", "upload"), the current rate in Mbit/s
// and what is already known (the exit address, then the download result).
type Progress func(phase string, mbps float64, sofar Result)

type Result struct {
	DownMbps float64 `json:"down_mbps"`
	UpMbps   float64 `json:"up_mbps"`
	IP       string  `json:"ip"`  // the address the test server saw: the VPN exit when tunnelled
	Loc      string  `json:"loc"` // its country code according to Cloudflare
}

// trace asks Cloudflare which address and country the request came from.
func trace(ctx context.Context, c *http.Client) (ip, loc string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/cdn-cgi/trace", nil)
	if err != nil {
		return "", "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("сервер теста недоступен: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, _ := strings.Cut(line, "=")
		switch k {
		case "ip":
			ip = v
		case "loc":
			loc = v
		}
	}
	return ip, loc, nil
}

func client(dial Dialer) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: dial,
		// Classic key shares only: the post-quantum one makes the ClientHello span two packets,
		// and Russian DPI drops such handshakes to Cloudflare when the test runs without the VPN.
		TLSClientConfig:     &tls.Config{ServerName: host, CurvePreferences: []tls.CurveID{tls.X25519, tls.CurveP256}},
		TLSHandshakeTimeout: 15 * time.Second,
		MaxIdleConnsPerHost: streams,
		DisableCompression:  true,
		ForceAttemptHTTP2:   false, // one TCP connection per stream
	}}
}

// Run measures download, then upload.
func Run(ctx context.Context, dial Dialer, progress Progress) (Result, error) {
	c := client(dial)
	defer c.CloseIdleConnections()
	var r Result
	var err error
	report := func(phase string, mbps float64) {
		if progress != nil {
			progress(phase, mbps, r)
		}
	}
	if r.IP, r.Loc, err = trace(ctx, c); err != nil {
		return r, err
	}
	report("connected", 0)
	if r.DownMbps, err = measure(ctx, "download", report, func(ctx context.Context, count *atomic.Int64) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://%s/__down?bytes=%d", host, downChunk), nil)
		if err != nil {
			return err
		}
		resp, err := c.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if err := statusErr(resp); err != nil {
			return err
		}
		_, err = io.Copy(countWriter{count}, resp.Body)
		return err
	}); err != nil {
		return r, err
	}
	r.UpMbps, err = measure(ctx, "upload", report, func(ctx context.Context, count *atomic.Int64) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+host+"/__up", &zeroReader{left: upChunk, count: count})
		if err != nil {
			return err
		}
		req.ContentLength = upChunk
		resp, err := c.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		return statusErr(resp)
	})
	return r, err
}

// errRateLimited: Cloudflare limits requests per address, and a VPN exit is shared by many users.
var errRateLimited = errors.New("Cloudflare временно ограничил тесты с адреса VPN — повторите через минуту")

func statusErr(resp *http.Response) error {
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return errRateLimited
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("сервер теста ответил HTTP %d", resp.StatusCode)
	}
	return nil
}

// measure runs transfer on several streams for phaseTime and returns Mbit/s after the warmup.
func measure(parent context.Context, phase string, progress func(string, float64), transfer func(context.Context, *atomic.Int64) error) (float64, error) {
	ctx, cancel := context.WithTimeout(parent, phaseTime)
	defer cancel()
	var count atomic.Int64
	var limited atomic.Bool
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	for i := 0; i < streams; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil { // a finished request is restarted until time is up
				err := transfer(ctx, &count)
				if errors.Is(err, errRateLimited) { // back off; the other streams keep measuring
					limited.Store(true)
					select {
					case <-ctx.Done():
					case <-time.After(700 * time.Millisecond):
					}
					continue
				}
				if err != nil && ctx.Err() == nil {
					// A dropped connection (the server may close a finished upload) is retried;
					// the phase fails only if nothing at all got through.
					errOnce.Do(func() { firstErr = err })
					select {
					case <-ctx.Done():
					case <-time.After(300 * time.Millisecond):
					}
				}
			}
		}()
	}
	start := time.Now()
	var base int64
	var baseAt time.Time
	t := time.NewTicker(tickPeriod)
	defer t.Stop()
	prev, prevAt := int64(0), start
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			if parent.Err() != nil {
				return 0, parent.Err()
			}
			if baseAt.IsZero() || count.Load() == 0 {
				switch {
				case limited.Load():
					return 0, errRateLimited
				case firstErr != nil:
					return 0, firstErr
				}
				return 0, errors.New("сервер теста скорости не ответил")
			}
			return mbps(count.Load()-base, time.Since(baseAt)), nil
		case now := <-t.C:
			n := count.Load()
			if baseAt.IsZero() && now.Sub(start) >= warmup {
				base, baseAt = n, now
			}
			if progress != nil {
				progress(phase, mbps(n-prev, now.Sub(prevAt)))
			}
			prev, prevAt = n, now
		}
	}
}

func mbps(bytes int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(bytes) * 8 / d.Seconds() / 1e6
}

type countWriter struct{ n *atomic.Int64 }

func (w countWriter) Write(p []byte) (int, error) {
	w.n.Add(int64(len(p)))
	return len(p), nil
}

// zeroReader is an upload body of left zero bytes that counts what the transport has taken.
type zeroReader struct {
	left  int64
	count *atomic.Int64
}

func (z *zeroReader) Read(p []byte) (int, error) {
	if z.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > z.left {
		p = p[:z.left]
	}
	clear(p)
	z.left -= int64(len(p))
	z.count.Add(int64(len(p)))
	return len(p), nil
}

// State is what the UI shows while and after a test runs.
type State struct {
	Running  bool      `json:"running"`
	Phase    string    `json:"phase"` // "connect", "download", "upload", "" when finished
	Mbps     float64   `json:"mbps"`  // current rate of the running phase
	DownMbps float64   `json:"down_mbps"`
	UpMbps   float64   `json:"up_mbps"`
	IP       string    `json:"ip"`
	Loc      string    `json:"loc"`
	Error    string    `json:"error"`
	At       time.Time `json:"at"`
}

// Tracker runs one test at a time and keeps its live state.
type Tracker struct {
	mu sync.Mutex
	st State
}

// Start runs the test in the background; it returns false if one is already running.
func (t *Tracker) Start(dial Dialer, done func(Result, error)) bool {
	t.mu.Lock()
	if t.st.Running {
		t.mu.Unlock()
		return false
	}
	t.st = State{Running: true, Phase: "connect", At: time.Now()}
	t.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		r, err := Run(ctx, dial, func(phase string, mbps float64, sofar Result) {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.st.IP, t.st.Loc, t.st.DownMbps = sofar.IP, sofar.Loc, sofar.DownMbps
			t.st.Phase, t.st.Mbps = phase, mbps
		})
		t.mu.Lock()
		t.st = State{DownMbps: r.DownMbps, UpMbps: r.UpMbps, IP: r.IP, Loc: r.Loc, At: time.Now()}
		if err != nil {
			t.st.Error = err.Error()
		}
		t.mu.Unlock()
		if done != nil {
			done(r, err)
		}
	}()
	return true
}

// Snapshot returns the live state.
func (t *Tracker) Snapshot() State {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.st
}
