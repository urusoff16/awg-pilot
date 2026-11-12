package xray

import (
	"encoding/binary"
	"errors"
	"strings"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Binder pins the sockets of Xray to the physical interface while a tunnel holds the default
// route, as amneziawg-go does with its UDP socket. It implements amneziawg-go's
// conn.BindSocketToInterface, so the tunnel's route monitor drives it the same way.
type Binder struct {
	index4, index6 atomic.Uint32
	hole4, hole6   atomic.Bool // no physical route: refuse instead of looping into the tunnel
}

func (b *Binder) BindSocketToInterface4(index uint32, blackhole bool) error {
	b.index4.Store(index)
	b.hole4.Store(blackhole)
	return nil
}

func (b *Binder) BindSocketToInterface6(index uint32, blackhole bool) error {
	b.index6.Store(index)
	b.hole6.Store(blackhole)
	return nil
}

var errNoNetwork = errors.New("нет подключения к сети")

// Hook is the SocketHook that applies the binding.
func (b *Binder) Hook(network string, fd uintptr) error {
	h := windows.Handle(fd)
	if strings.HasSuffix(network, "6") {
		if b.hole6.Load() {
			return errNoNetwork
		}
		if i := b.index6.Load(); i != 0 {
			return windows.SetsockoptInt(h, windows.IPPROTO_IPV6, 31 /* IPV6_UNICAST_IF */, int(i))
		}
		return nil
	}
	if b.hole4.Load() {
		return errNoNetwork
	}
	i := b.index4.Load()
	if i == 0 {
		return nil
	}
	// For IPv4 the index goes in network byte order.
	var be [4]byte
	binary.BigEndian.PutUint32(be[:], i)
	return windows.SetsockoptInt(h, windows.IPPROTO_IP, 31 /* IP_UNICAST_IF */, int(*(*uint32)(unsafe.Pointer(&be[0]))))
}
