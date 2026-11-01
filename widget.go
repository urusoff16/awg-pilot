package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
	"unsafe"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
	"golang.org/x/sys/windows"

	"awgpilot/internal/i18n"
	"awgpilot/internal/ipc"
)

const (
	widgetTitle  = "AWG Pilot — статус"
	widgetWidth  = 300
	widgetHeight = 96
)

// Widget is the small always-on-top status window, run as `awgpilot.exe --widget`. It reads the
// tunnel state straight from the service, so it is accurate even when the main window is closed.
type Widget struct {
	ctx context.Context
	c   ipc.Client

	mu       sync.Mutex
	prevT    time.Time
	prevRx   uint64
	prevTx   uint64
	pingSeen bool
	polls    int
	lastX    int
	lastY    int
}

// WidgetStatus is what the status window shows. State: ok, warn, bad, off.
type WidgetStatus struct {
	State  string  `json:"state"`
	Title  string  `json:"title"`
	Detail string  `json:"detail"`
	Rx     float64 `json:"rx"`   // bytes per second, for the graph
	Tx     float64 `json:"tx"`   //
	PingMs int64   `json:"ping"` // -1 = no reply
	Lang   string  `json:"lang"` // interface language: "ru" or "en"
}

func (w *Widget) startup(ctx context.Context) { w.ctx = ctx }

// domReady positions the window and hides its taskbar button (a tool window).
func (w *Widget) domReady(ctx context.Context) {
	x, y, ok := loadWidgetPos()
	if !ok {
		if screens, err := wruntime.ScreenGetAll(ctx); err == nil {
			for _, s := range screens {
				if s.IsPrimary {
					x, y = s.Size.Width-widgetWidth-24, s.Size.Height-widgetHeight-72
				}
			}
		}
	}
	wruntime.WindowSetPosition(ctx, x, y)
	hideFromTaskbar(i18n.T(widgetTitle))
}

func (w *Widget) shutdown(ctx context.Context) {
	x, y := wruntime.WindowGetPosition(ctx)
	saveWidgetPos(x, y)
}

// Status computes the colour and text from the service state, in the interface language.
func (w *Widget) Status() WidgetStatus {
	st := w.status()
	st.Title, st.Detail, st.Lang = i18n.T(st.Title), i18n.T(st.Detail), i18n.Lang()
	return st
}

func (w *Widget) status() WidgetStatus {
	if _, err := w.c.Version(); err != nil {
		return WidgetStatus{State: "bad", Title: "Служба не запущена", Detail: "переустановите AWG Pilot"}
	}
	st := w.c.Stats()
	now := time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.polls++; w.polls%5 == 0 && w.ctx != nil {
		if x, y := wruntime.WindowGetPosition(w.ctx); x != w.lastX || y != w.lastY {
			w.lastX, w.lastY = x, y
			saveWidgetPos(x, y)
		}
	}
	if !st.Up {
		w.prevT, w.pingSeen = time.Time{}, false
		if st.Blocked {
			return WidgetStatus{State: "bad", Title: "Интернет заблокирован", Detail: "VPN оборвался — восстанавливаю"}
		}
		return WidgetStatus{State: "off", Title: "VPN отключён", Detail: "интернет напрямую"}
	}
	var rx, tx float64
	if !w.prevT.IsZero() && st.RxBytes >= w.prevRx && st.TxBytes >= w.prevTx {
		dt := now.Sub(w.prevT).Seconds()
		if dt > 0 {
			rx, tx = float64(st.RxBytes-w.prevRx)/dt, float64(st.TxBytes-w.prevTx)/dt
		}
	}
	w.prevT, w.prevRx, w.prevTx = now, st.RxBytes, st.TxBytes
	if st.PingMs >= 0 {
		w.pingSeen = true
	}
	switch {
	case st.LastHandshake.IsZero() && time.Since(st.Since) < 20*time.Second:
		return WidgetStatus{State: "warn", Title: "Подключение…", Detail: "ожидание ответа сервера"}
	case st.LastHandshake.IsZero() || time.Since(st.LastHandshake) > staleAfter || (w.pingSeen && st.PingFails >= 3):
		return WidgetStatus{State: "bad", Title: "Нет связи с сервером", Detail: "восстанавливаю подключение"}
	}
	ping := "пинг —"
	if st.PingMs >= 0 {
		ping = fmt.Sprintf("%d мс", st.PingMs)
	}
	return WidgetStatus{State: "ok", Title: "VPN подключён", Detail: fmt.Sprintf("%s · ↓ %s/с · ↑ %s/с", ping, humanBytes(rx), humanBytes(tx)),
		Rx: rx, Tx: tx, PingMs: st.PingMs}
}

// OpenMain shows the main window (a second start of awgpilot.exe brings it to the front).
func (w *Widget) OpenMain() {
	if exe, err := os.Executable(); err == nil {
		exec.Command(exe).Start()
	}
}

// Close hides the status window (the main app notices the exit and unticks the tray item).
func (w *Widget) Close() { wruntime.Quit(w.ctx) }

// ---- position persistence and window style ------------------------------------------------

func widgetPosPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "AWGPilot", "widget.json")
}

func loadWidgetPos() (int, int, bool) {
	var p struct{ X, Y int }
	b, err := os.ReadFile(widgetPosPath())
	if err != nil || json.Unmarshal(b, &p) != nil {
		return 0, 0, false
	}
	return p.X, p.Y, true
}

func saveWidgetPos(x, y int) {
	if path := widgetPosPath(); path != "" {
		b, _ := json.Marshal(struct{ X, Y int }{x, y})
		os.WriteFile(path, b, 0o600)
	}
}

var (
	user32              = windows.NewLazySystemDLL("user32.dll")
	procFindWindowW     = user32.NewProc("FindWindowW")
	procGetWindowLng    = user32.NewProc("GetWindowLongPtrW")
	procSetWindowLng    = user32.NewProc("SetWindowLongPtrW")
	procShowWindow      = user32.NewProc("ShowWindow")
	procSetWindowPos    = user32.NewProc("SetWindowPos")
	procIsWindowVisible = user32.NewProc("IsWindowVisible")
)

const (
	gwlStyle         = ^uintptr(15) // GWL_STYLE = -16
	gwlExStyle       = ^uintptr(19) // GWL_EXSTYLE = -20
	wsMaximizeBox    = 0x00010000
	wsExToolWindow   = 0x00000080
	wsExAppWindow    = 0x00040000
	swHide           = 0
	swShowNoActivate = 4
	findWindowTries  = 20
)

// noMaximize removes the maximize button of the window with this title. Without it Windows also
// ignores double-clicks on the title bar and Win+Up, and does not snap the window to full screen.
func noMaximize(title string) {
	t, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return
	}
	// Wait until Wails has shown the window: changing the style of a window that is being shown
	// at that moment could write back a style without WS_VISIBLE and leave it hidden.
	for i := 0; i < findWindowTries*10; i++ {
		hwnd, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(t)))
		if visible, _, _ := procIsWindowVisible.Call(hwnd); hwnd != 0 && visible != 0 {
			style, _, _ := procGetWindowLng.Call(hwnd, gwlStyle)
			if style&wsMaximizeBox == 0 {
				return
			}
			procSetWindowLng.Call(hwnd, gwlStyle, style&^wsMaximizeBox)
			// SWP_NOSIZE|SWP_NOMOVE|SWP_NOZORDER|SWP_NOACTIVATE|SWP_FRAMECHANGED: redraw the caption
			procSetWindowPos.Call(hwnd, 0, 0, 0, 0, 0, 0x0001|0x0002|0x0004|0x0010|0x0020)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// hideFromTaskbar turns the status window into a tool window, so it has no taskbar button.
// The taskbar only notices the style change when the window is shown again, hence hide/show.
func hideFromTaskbar(title string) {
	t, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return
	}
	for i := 0; i < findWindowTries; i++ {
		hwnd, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(t)))
		if hwnd != 0 {
			procShowWindow.Call(hwnd, swHide)
			style, _, _ := procGetWindowLng.Call(hwnd, gwlExStyle)
			style = (style | wsExToolWindow) &^ wsExAppWindow
			procSetWindowLng.Call(hwnd, gwlExStyle, style)
			procShowWindow.Call(hwnd, swShowNoActivate)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
