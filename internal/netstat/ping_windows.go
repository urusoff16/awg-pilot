// Package netstat measures tunnel throughput and latency.
package netstat

import (
	"errors"
	"fmt"
	"net/netip"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	iphlpapi          = windows.NewLazySystemDLL("iphlpapi.dll")
	procIcmpCreate    = iphlpapi.NewProc("IcmpCreateFile")
	procIcmpClose     = iphlpapi.NewProc("IcmpCloseHandle")
	procIcmpSendEcho  = iphlpapi.NewProc("IcmpSendEcho")
	errPingNotIPv4    = errors.New("пинг поддерживается только для IPv4")
	invalidHandleICMP = ^uintptr(0)
)

// Ping sends one ICMP echo with the system ICMP API (no raw socket or admin rights needed)
// and returns the round-trip time reported by Windows.
func Ping(addr netip.Addr, timeout time.Duration) (time.Duration, error) {
	if !addr.Is4() {
		return 0, errPingNotIPv4
	}
	h, _, err := procIcmpCreate.Call()
	if h == invalidHandleICMP || h == 0 {
		return 0, fmt.Errorf("IcmpCreateFile: %w", err)
	}
	defer procIcmpClose.Call(h)

	b := addr.As4()
	dst := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24 // IPAddr is in network byte order
	payload := []byte("awgpilot-ping")
	// ICMP_ECHO_REPLY (40 bytes on 64-bit) + payload + 8 bytes for an ICMP error, as documented.
	reply := make([]byte, 64+len(payload)+8)
	start := time.Now()
	n, _, err := procIcmpSendEcho.Call(h, uintptr(dst),
		uintptr(unsafe.Pointer(&payload[0])), uintptr(len(payload)), 0,
		uintptr(unsafe.Pointer(&reply[0])), uintptr(len(reply)), uintptr(timeout.Milliseconds()))
	if n == 0 {
		return 0, fmt.Errorf("нет ответа: %w", err)
	}
	status := *(*uint32)(unsafe.Pointer(&reply[4]))
	if status != 0 {
		return 0, fmt.Errorf("ICMP статус %d", status)
	}
	rtt := time.Duration(*(*uint32)(unsafe.Pointer(&reply[8]))) * time.Millisecond
	if rtt == 0 { // sub-millisecond replies are reported as 0
		rtt = time.Since(start)
	}
	return rtt, nil
}
