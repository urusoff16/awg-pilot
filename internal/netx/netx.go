// Package netx provides name resolution and HTTP for AWG Pilot's own requests that keep working
// while the "internet off when the VPN drops" lockdown is active. The lockdown lets only this
// program through, but the system DNS client (a separate svchost service) is blocked, so names
// are resolved in-process by Go's resolver instead.
package netx

import (
	"net"
	"net/http"
	"runtime"
	"time"
)

// Resolver sends DNS queries from this process, using the system's configured DNS servers.
// Android has no resolv.conf, so there it keeps the system (netd) resolver.
var Resolver = &net.Resolver{PreferGo: runtime.GOOS != "android"}

var dialer = &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second, Resolver: Resolver}

// Transport is an http.Transport whose dials resolve names with Resolver.
func Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = dialer.DialContext
	return t
}

// Client is a shared HTTP client built on Transport.
var Client = &http.Client{Timeout: 60 * time.Second, Transport: Transport()}
