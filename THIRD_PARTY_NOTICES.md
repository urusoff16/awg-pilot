# Third-party notices

AWG Pilot uses the following components. Their licenses apply to those components only.

| Component | Used for | License |
|---|---|---|
| [amneziawg-go](https://github.com/amnezia-vpn/amneziawg-go) v3 | AmneziaWG engine (Windows and Android) | MIT, © WireGuard LLC |
| [amneziawg-windows](https://github.com/amnezia-vpn/amneziawg-windows) v0.2.0 | Config parser, interface, route and DNS setup; `internal/tunnel` and `internal/wfp` adapt parts of its `tunnel` and `tunnel/firewall` packages | MIT, © WireGuard LLC |
| [WinTun](https://www.wintun.net) 0.14.1 (`wintun.dll`) | Virtual network adapter on Windows, shipped unmodified | WinTun Prebuilt Binaries License, © WireGuard LLC |
| [Wails](https://wails.io) v2 | Windows desktop shell | MIT, © Lea Anthony |
| [Xray-core](https://github.com/XTLS/Xray-core) v26.11.9 | VLESS, Trojan, Shadowsocks and VMess engine with REALITY, TLS, WebSocket, gRPC, XHTTP and its TUN TCP/IP stack (Windows and Android), used unmodified as a library | Mozilla Public License 2.0, © Project X |
| [REALITY](https://github.com/XTLS/REALITY), [uTLS](https://github.com/refraction-networking/utls), [quic-go](https://github.com/quic-go/quic-go) and the other modules Xray-core depends on | Parts of Xray-core | MPL 2.0, BSD 3-Clause, MIT and Apache 2.0 (see each module) |
| [gVisor](https://gvisor.dev) netstack | In-tunnel TCP stack for the Android speed test | Apache License 2.0, © The gVisor Authors |
| [fyne.io/systray](https://github.com/fyne-io/systray) | Windows tray icon | Apache License 2.0 |
| [Go x/sys, x/net](https://go.dev) | System calls, DNS message parsing | BSD 3-Clause, © The Go Authors |

Xray-core is covered by the Mozilla Public License 2.0: its source code is available at https://github.com/XTLS/Xray-core (tag v26.11.9), and AWG Pilot uses its files unchanged.

The full license texts are included in each component's source distribution (the Go module cache, `go mod vendor`, or the linked repositories).

WireGuard is a registered trademark of Jason A. Donenfeld. AmneziaWG is a project of Amnezia. AWG Pilot is not affiliated with either.
