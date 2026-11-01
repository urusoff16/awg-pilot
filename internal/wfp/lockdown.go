package wfp

import (
	"fmt"
	"sync"
)

// Lockdown keeps the internet closed while the VPN is down ("disconnect the internet when the
// VPN drops"). It lives in its own dynamic WFP session, independent of the tunnel's kill switch,
// so it survives the tunnel going away. It permits only this program (so it can reconnect and
// reach the provider's API), loopback, DHCP and neighbour discovery, plus the tunnel adapter
// when one is up (tunLUID != 0).
type Lockdown struct {
	mu      sync.Mutex
	session uintptr
	luid    uint64
}

// Apply (re)installs the lockdown for tunLUID (0 = no tunnel: block everything else). The new
// session is committed before the old one closes, so protection never lapses.
func (l *Lockdown) Apply(tunLUID uint64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.session != 0 && l.luid == tunLUID {
		return nil
	}
	session, err := createWfpSession()
	if err != nil {
		return fmt.Errorf("не удалось открыть сеанс фильтров Windows (WFP): %w", err)
	}
	err = runTransaction(session, func(session uintptr) error {
		bo, err := registerBaseObjects(session)
		if err != nil {
			return err
		}
		if err := permitWireGuardService(session, bo, 15); err != nil {
			return err
		}
		if err := permitLoopback(session, bo, 13); err != nil {
			return err
		}
		if tunLUID != 0 {
			if err := permitTunInterface(session, bo, 12, tunLUID); err != nil {
				return err
			}
		}
		for _, permit := range []func(uintptr, *baseObjects, uint8) error{permitDHCPIPv4, permitDHCPIPv6, permitNdp} {
			if err := permit(session, bo, 12); err != nil {
				return err
			}
		}
		return blockAll(session, bo, 0)
	})
	if err != nil {
		fwpmEngineClose0(session)
		return fmt.Errorf("не удалось включить блокировку интернета: %w", err)
	}
	if l.session != 0 {
		fwpmEngineClose0(l.session)
	}
	l.session, l.luid = session, tunLUID
	return nil
}

// Close lifts the lockdown.
func (l *Lockdown) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.session != 0 {
		fwpmEngineClose0(l.session)
		l.session, l.luid = 0, 0
	}
}

// Active reports whether the lockdown filters are installed.
func (l *Lockdown) Active() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.session != 0
}
