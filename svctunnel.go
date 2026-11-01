package main

import (
	"awgpilot/internal/ipc"
	"awgpilot/internal/tunnel"
)

// svcTunnel gives the window the same tunnel API it had in-process, backed by the service.
type svcTunnel struct{ c ipc.Client }

func (t *svcTunnel) Up(o tunnel.Options) error {
	return t.c.Up(ipc.UpParams{
		RawConfig: o.RawConfig, AllowedIPs: o.AllowedIPs, FullTunnel: o.FullTunnel,
		KillSwitch: o.KillSwitch, BlockIPv6: o.BlockIPv6, DNS: o.DNS, MTU: o.MTU,
		Lockdown: o.Lockdown,
	})
}

func (t *svcTunnel) Down(release bool)   { t.c.Down(release) }
func (t *svcTunnel) Unlock() error       { return t.c.Unlock() }
func (t *svcTunnel) Stats() tunnel.Stats { return t.c.Stats() }
func (t *svcTunnel) IsUp() bool          { return t.c.Stats().Up }

// Available reports whether the service answers on its pipe.
func (t *svcTunnel) Available() bool {
	_, err := t.c.Version()
	return err == nil
}
