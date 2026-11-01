// Package ipc connects the unprivileged AWG Pilot window to the AWG Pilot Windows service,
// which owns the tunnel, routes and firewall rules. One JSON request and one JSON response
// travel over each named-pipe connection.
package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"time"
	"unsafe"

	"github.com/amnezia-vpn/amneziawg-go/v3/ipc/namedpipe"
	"golang.org/x/sys/windows"

	"awgpilot/internal/flows"
	"awgpilot/internal/tunnel"
)

// PipePath is the service's control pipe.
const PipePath = `\\.\pipe\AWGPilot`

// pipeSDDL lets SYSTEM, Administrators and interactively logged-on users connect; nobody else
// (services, network logons) can drive the tunnel.
const pipeSDDL = "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GRGW;;;IU)"

// UpParams describes the tunnel to bring up (tunnel.Options without the logger).
type UpParams struct {
	RawConfig  string         `json:"raw_config"`
	AllowedIPs []netip.Prefix `json:"allowed_ips"`
	FullTunnel bool           `json:"full_tunnel"`
	KillSwitch bool           `json:"kill_switch"`
	BlockIPv6  bool           `json:"block_ipv6"`
	DNS        []netip.Addr   `json:"dns"`
	MTU        int            `json:"mtu"`
	// Lockdown keeps the internet closed if the tunnel drops (until a manual disconnect).
	Lockdown bool `json:"lockdown"`
}

type Request struct {
	Op    string    `json:"op"` // version, up, down, stats, apps, logs, unlock, flows, flowlog, flowclear
	Up    *UpParams `json:"up,omitempty"`
	Apps  []string  `json:"apps,omitempty"`
	Since int       `json:"since,omitempty"`
	// Release lifts the lockdown on "down": the user disconnected on purpose.
	Release bool   `json:"release,omitempty"`
	Day     string `json:"day,omitempty"` // flows: which day ("" = today)
	On      bool   `json:"on,omitempty"`  // flowlog: record or not
}

type Response struct {
	Error   string        `json:"error,omitempty"`
	Version string        `json:"version,omitempty"`
	Stats   *tunnel.Stats `json:"stats,omitempty"`
	Logs    []string      `json:"logs,omitempty"`
	Next    int           `json:"next,omitempty"`
	Flows   *flows.Report `json:"flows,omitempty"`
}

// Backend is what the service exposes over the pipe.
type Backend interface {
	Up(UpParams) error
	Down(release bool)
	Unlock()
	Stats() tunnel.Stats
	SetApps([]string) error
	Logs(since int) ([]string, int)
	Flows(day string) flows.Report
	SetFlowLog(on bool) error
	ClearFlows() error
}

// ---- server -------------------------------------------------------------------------------

// Serve answers requests on path until ctx is cancelled.
func Serve(ctx context.Context, path string, b Backend, version string) error {
	sd, err := windows.SecurityDescriptorFromString(pipeSDDL)
	if err != nil {
		return err
	}
	l, err := (&namedpipe.ListenConfig{SecurityDescriptor: sd}).Listen(path)
	if err != nil {
		return fmt.Errorf("не удалось открыть канал управления: %w", err)
	}
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		go handle(c, b, version)
	}
}

func handle(c net.Conn, b Backend, version string) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Minute)) // bringing up ~20k routes takes a while
	var req Request
	if err := json.NewDecoder(io.LimitReader(c, 64<<20)).Decode(&req); err != nil {
		return
	}
	json.NewEncoder(c).Encode(dispatch(req, b, version))
}

func dispatch(req Request, b Backend, version string) Response {
	switch req.Op {
	case "version":
		return Response{Version: version}
	case "up":
		if req.Up == nil {
			return Response{Error: "нет параметров туннеля"}
		}
		if err := b.Up(*req.Up); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{}
	case "down":
		b.Down(req.Release)
		return Response{}
	case "unlock":
		b.Unlock()
		return Response{}
	case "stats":
		st := b.Stats()
		return Response{Stats: &st}
	case "apps":
		if err := b.SetApps(req.Apps); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{}
	case "logs":
		lines, next := b.Logs(req.Since)
		return Response{Logs: lines, Next: next}
	case "flows":
		rep := b.Flows(req.Day)
		return Response{Flows: &rep}
	case "flowlog":
		if err := b.SetFlowLog(req.On); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{}
	case "flowclear":
		if err := b.ClearFlows(); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{}
	}
	return Response{Error: "неизвестная команда " + req.Op}
}

// ---- client -------------------------------------------------------------------------------

// ErrNoService means the pipe is absent: the service is not installed or not running.
var ErrNoService = errors.New("служба «AWG Pilot VPN» не запущена — переустановите программу или запустите службу в «Службах» Windows")

type Client struct {
	Path string // defaults to PipePath
}

func (c *Client) call(req Request, timeout time.Duration) (*Response, error) {
	path := c.Path
	if path == "" {
		path = PipePath
	}
	conn, err := namedpipe.DialTimeout(path, 3*time.Second)
	if err != nil {
		return nil, ErrNoService
	}
	defer conn.Close()
	if c.Path == "" {
		if err := verifyServer(conn); err != nil {
			return nil, err
		}
	}
	conn.SetDeadline(time.Now().Add(timeout))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, fmt.Errorf("служба не приняла запрос: %w", err)
	}
	var resp Response
	if err := json.NewDecoder(io.LimitReader(conn, 16<<20)).Decode(&resp); err != nil {
		return nil, fmt.Errorf("служба не ответила: %w", err)
	}
	if resp.Error != "" {
		return &resp, errors.New(resp.Error)
	}
	return &resp, nil
}

// verifyServer checks that the pipe was created by the AWG Pilot service. While the service is
// stopped its pipe name is free, and another local program could take it to receive the tunnel
// config with its private key. Objects created by a LocalSystem service are owned by SYSTEM or
// Administrators, which an ordinary user cannot fake.
func verifyServer(conn net.Conn) error {
	f, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return errors.New("не удалось проверить канал службы")
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("не удалось проверить владельца канала службы: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return errors.New("не удалось проверить владельца канала службы")
	}
	trusted := false
	for _, t := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid} {
		if want, err := windows.CreateWellKnownSid(t); err == nil && owner.Equals(want) {
			trusted = true
		}
	}
	if !trusted {
		return errors.New("канал службы AWG Pilot создан посторонней программой — подключение отменено")
	}
	// The pipe's ACL lets users open it for writing, which also lets them add their own server
	// instance under the same name and owner. Services run in session 0 and user programs never
	// do, so the instance that accepted us must belong to a session-0 process.
	var session uint32
	if r, _, err := procGetNamedPipeServerSessionID.Call(f.Fd(), uintptr(unsafe.Pointer(&session))); r == 0 {
		return fmt.Errorf("не удалось проверить канал службы: %w", err)
	}
	if session != 0 {
		return errors.New("канал службы AWG Pilot обслуживает посторонняя программа — подключение отменено")
	}
	return nil
}

var procGetNamedPipeServerSessionID = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetNamedPipeServerSessionId")

func (c *Client) Version() (string, error) {
	r, err := c.call(Request{Op: "version"}, 5*time.Second)
	if err != nil {
		return "", err
	}
	return r.Version, nil
}

func (c *Client) Up(p UpParams) error {
	_, err := c.call(Request{Op: "up", Up: &p}, 3*time.Minute)
	return err
}

// Down stops the tunnel. release=true is a deliberate disconnect and also lifts the lockdown;
// release=false (reconnects, profile switches) keeps the internet closed if the lockdown is armed.
func (c *Client) Down(release bool) error {
	_, err := c.call(Request{Op: "down", Release: release}, time.Minute)
	return err
}

// Unlock reopens the internet after a VPN drop without connecting.
func (c *Client) Unlock() error {
	_, err := c.call(Request{Op: "unlock"}, time.Minute)
	return err
}

// Stats returns the tunnel state; an unreachable service reads as "down".
func (c *Client) Stats() tunnel.Stats {
	r, err := c.call(Request{Op: "stats"}, 5*time.Second)
	if err != nil || r.Stats == nil {
		return tunnel.Stats{}
	}
	return *r.Stats
}

func (c *Client) SetApps(apps []string) error {
	_, err := c.call(Request{Op: "apps", Apps: apps}, time.Minute)
	return err
}

func (c *Client) Logs(since int) ([]string, int, error) {
	r, err := c.call(Request{Op: "logs", Since: since}, 5*time.Second)
	if err != nil {
		return nil, since, err
	}
	return r.Logs, r.Next, nil
}

// Flows returns the traffic log of one day ("" = today).
func (c *Client) Flows(day string) (flows.Report, error) {
	r, err := c.call(Request{Op: "flows", Day: day}, 10*time.Second)
	if err != nil {
		return flows.Report{}, err
	}
	if r.Flows == nil {
		return flows.Report{}, errors.New("служба не поддерживает журнал трафика — обновите программу")
	}
	return *r.Flows, nil
}

// SetFlowLog turns the traffic log on or off.
func (c *Client) SetFlowLog(on bool) error {
	_, err := c.call(Request{Op: "flowlog", On: on}, 10*time.Second)
	return err
}

// ClearFlows deletes the traffic log.
func (c *Client) ClearFlows() error {
	_, err := c.call(Request{Op: "flowclear"}, 10*time.Second)
	return err
}
