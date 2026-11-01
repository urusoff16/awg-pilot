// Package procnet finds the program behind a local TCP/UDP port, the way Resource Monitor does:
// the system connection tables give the process ID, the process gives its executable name, and
// for svchost.exe the services running in it are added ("svchost.exe (DoSvc)").
package procnet

import (
	"encoding/binary"
	"net/netip"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	iphlpapi                = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedTcpTable = iphlpapi.NewProc("GetExtendedTcpTable")
	procGetExtendedUdpTable = iphlpapi.NewProc("GetExtendedUdpTable")
)

const (
	tcpTableOwnerPIDAll = 5
	udpTableOwnerPID    = 1
	tablesMaxAge        = 300 * time.Millisecond
	namesMaxAge         = 30 * time.Second
	servicesMaxAge      = time.Minute
)

type tcpKey struct {
	lport, rport uint16
	remote       netip.Addr
}

// Resolver caches the tables briefly: one burst of new connections costs one snapshot.
type Resolver struct {
	mu       sync.Mutex
	at       time.Time
	tcp      map[tcpKey]uint32
	tcpPort  map[uint16]uint32
	udp      map[uint16]uint32
	names    map[uint32]cached
	svcAt    time.Time
	services map[uint32][]string
}

type cached struct {
	name string
	at   time.Time
}

func New() *Resolver { return &Resolver{names: map[uint32]cached{}} }

// Owner names the program using lport (remote and rport narrow down TCP sockets; the local
// address is always the tunnel's and adds nothing).
func (r *Resolver) Owner(proto uint8, _ netip.Addr, lport uint16, remote netip.Addr, rport uint16) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.at) > tablesMaxAge {
		r.refresh()
	}
	var pid uint32
	var ok bool
	switch proto {
	case 6:
		if pid, ok = r.tcp[tcpKey{lport, rport, remote.Unmap()}]; !ok {
			pid, ok = r.tcpPort[lport]
		}
	case 17:
		pid, ok = r.udp[lport]
	}
	if !ok {
		return ""
	}
	return r.name(pid)
}

func (r *Resolver) refresh() {
	r.at = time.Now()
	r.tcp, r.tcpPort, r.udp = map[tcpKey]uint32{}, map[uint16]uint32{}, map[uint16]uint32{}
	// MIB_TCPROW_OWNER_PID: state, local addr, local port, remote addr, remote port, pid
	if b := table(procGetExtendedTcpTable, windows.AF_INET, tcpTableOwnerPIDAll); b != nil {
		rows(b, 24, func(e []byte) {
			lport, rport := port(e[8:]), port(e[16:])
			remote := netip.AddrFrom4([4]byte(e[12:16]))
			pid := binary.LittleEndian.Uint32(e[20:])
			r.tcp[tcpKey{lport, rport, remote}] = pid
			r.tcpPort[lport] = pid
		})
	}
	// MIB_TCP6ROW_OWNER_PID: local addr[16], scope, local port, remote addr[16], scope, remote port, state, pid
	if b := table(procGetExtendedTcpTable, windows.AF_INET6, tcpTableOwnerPIDAll); b != nil {
		rows(b, 56, func(e []byte) {
			lport, rport := port(e[20:]), port(e[44:])
			remote := netip.AddrFrom16([16]byte(e[24:40])).Unmap()
			pid := binary.LittleEndian.Uint32(e[52:])
			r.tcp[tcpKey{lport, rport, remote}] = pid
			if _, seen := r.tcpPort[lport]; !seen {
				r.tcpPort[lport] = pid
			}
		})
	}
	// MIB_UDPROW_OWNER_PID: local addr, local port, pid
	if b := table(procGetExtendedUdpTable, windows.AF_INET, udpTableOwnerPID); b != nil {
		rows(b, 12, func(e []byte) { r.udp[port(e[4:])] = binary.LittleEndian.Uint32(e[8:]) })
	}
	// MIB_UDP6ROW_OWNER_PID: local addr[16], scope, local port, pid
	if b := table(procGetExtendedUdpTable, windows.AF_INET6, udpTableOwnerPID); b != nil {
		rows(b, 28, func(e []byte) {
			if p := port(e[20:]); r.udp[p] == 0 {
				r.udp[p] = binary.LittleEndian.Uint32(e[24:])
			}
		})
	}
}

// table calls GetExtendedTcpTable/GetExtendedUdpTable, growing the buffer as asked.
func table(proc *windows.LazyProc, family, class uint32) []byte {
	size := uint32(64 << 10)
	for i := 0; i < 4; i++ {
		buf := make([]byte, size)
		ret, _, _ := proc.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, uintptr(family), uintptr(class), 0)
		switch ret {
		case 0:
			return buf[:size]
		case uintptr(windows.ERROR_INSUFFICIENT_BUFFER):
			size += 16 << 10 // the table may grow between the two calls
		default:
			return nil
		}
	}
	return nil
}

// rows walks a MIB_*TABLE_OWNER_PID: a uint32 count followed by fixed-size rows.
func rows(b []byte, size int, fn func([]byte)) {
	if len(b) < 4 {
		return
	}
	n := int(binary.LittleEndian.Uint32(b))
	for i := 0; i < n && 4+(i+1)*size <= len(b); i++ {
		fn(b[4+i*size : 4+(i+1)*size])
	}
}

// port reads a DWORD port field: network byte order in its low two bytes.
func port(b []byte) uint16 { return binary.BigEndian.Uint16(b[:2]) }

func (r *Resolver) name(pid uint32) string {
	switch pid {
	case 0:
		return ""
	case 4:
		return "System"
	}
	if c, ok := r.names[pid]; ok && time.Since(c.at) < namesMaxAge {
		return c.name
	}
	n := exeName(pid)
	if n == "" { // protected or already gone
		n = "процесс " + strconv.FormatUint(uint64(pid), 10)
	}
	if strings.EqualFold(n, "svchost.exe") {
		if s := r.servicesOf(pid); len(s) > 0 {
			if len(s) > 3 {
				s = append(s[:3:3], "…")
			}
			n += " (" + strings.Join(s, ", ") + ")"
		}
	}
	if len(r.names) > 2000 {
		r.names = map[uint32]cached{}
	}
	r.names[pid] = cached{n, time.Now()}
	return n
}

func exeName(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if windows.QueryFullProcessImageName(h, 0, &buf[0], &size) != nil {
		return ""
	}
	return filepath.Base(windows.UTF16ToString(buf[:size]))
}

func (r *Resolver) servicesOf(pid uint32) []string {
	if r.services == nil || time.Since(r.svcAt) > servicesMaxAge {
		r.svcAt = time.Now()
		r.services = runningServices()
	}
	return r.services[pid]
}

// runningServices maps process IDs to the names of the services they host.
func runningServices() map[uint32][]string {
	out := map[uint32][]string{}
	m, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_ENUMERATE_SERVICE)
	if err != nil {
		return out
	}
	defer windows.CloseServiceHandle(m)
	var need, count, resume uint32
	buf := make([]byte, 64<<10)
	for i := 0; i < 20; i++ {
		err = windows.EnumServicesStatusEx(m, windows.SC_ENUM_PROCESS_INFO, windows.SERVICE_WIN32, windows.SERVICE_ACTIVE,
			&buf[0], uint32(len(buf)), &need, &count, &resume, nil)
		if err != nil && err != windows.ERROR_MORE_DATA {
			return out
		}
		if count > 0 {
			list := unsafe.Slice((*windows.ENUM_SERVICE_STATUS_PROCESS)(unsafe.Pointer(&buf[0])), count)
			for _, s := range list {
				pid := s.ServiceStatusProcess.ProcessId
				out[pid] = append(out[pid], windows.UTF16PtrToString(s.ServiceName))
			}
		}
		if err == nil {
			break
		}
		if need > uint32(len(buf)) {
			buf = make([]byte, need)
		}
	}
	for _, s := range out {
		sort.Strings(s)
	}
	return out
}
