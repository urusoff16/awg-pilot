// Package tray shows a notification-area icon with a status tooltip and a small menu.
package tray

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"runtime"
	"sync"

	"fyne.io/systray"

	"awgpilot/internal/i18n"
)

type Callbacks struct {
	Show          func()
	Toggle        func() // connect or disconnect
	Quit          func()
	SelectProfile func(index int)
	ToggleWidget  func()
}

// maxProfiles is the number of pre-created profile items; menus are not rebuilt at runtime.
const maxProfiles = 10

type Tray struct {
	mu        sync.Mutex
	ready     bool
	connected bool
	tooltip   string
	open      *systray.MenuItem
	quit      *systray.MenuItem
	toggle    *systray.MenuItem
	widget    *systray.MenuItem
	widgetOn  bool
	profMenu  *systray.MenuItem
	profItems []*systray.MenuItem
	profNames []string
	profAct   int
	iconOn    []byte
	iconOff   []byte
}

// Start runs the tray on its own locked OS thread: the hidden tray window and its message
// loop must share a thread, separate from the Wails UI thread.
func Start(cb Callbacks) *Tray {
	t := &Tray{
		iconOn:  circleICO(color.RGBA{63, 182, 139, 255}),
		iconOff: circleICO(color.RGBA{128, 138, 150, 255}),
		tooltip: i18n.T("AWG Pilot — отключено"),
	}
	go func() {
		runtime.LockOSThread()
		systray.Run(func() { t.onReady(cb) }, nil)
	}()
	return t
}

func (t *Tray) onReady(cb Callbacks) {
	systray.SetIcon(t.iconOff)
	systray.SetTooltip(t.tooltip)
	systray.SetOnTapped(cb.Show)

	open := systray.AddMenuItem(i18n.T("Открыть AWG Pilot"), "")
	t.mu.Lock()
	t.open = open
	t.toggle = systray.AddMenuItem(i18n.T("Подключить"), "")
	t.widget = systray.AddMenuItemCheckbox(i18n.T("Окошко статуса"), i18n.T("Маленькое окно поверх других: зелёное — работает, красное — связь прервана"), false)
	go func() {
		for range t.widget.ClickedCh {
			if cb.ToggleWidget != nil {
				cb.ToggleWidget()
			}
		}
	}()
	t.profMenu = systray.AddMenuItem(i18n.T("Профиль"), i18n.T("Переключить ключ или конфиг"))
	for i := 0; i < maxProfiles; i++ {
		it := t.profMenu.AddSubMenuItemCheckbox("", "", false)
		it.Hide()
		t.profItems = append(t.profItems, it)
		go func(i int, it *systray.MenuItem) {
			for range it.ClickedCh {
				if cb.SelectProfile != nil {
					cb.SelectProfile(i)
				}
			}
		}(i, it)
	}
	t.mu.Unlock()
	systray.AddSeparator()
	quit := systray.AddMenuItem(i18n.T("Выход"), i18n.T("Отключить VPN и закрыть программу"))

	t.mu.Lock()
	t.quit = quit
	t.ready = true
	t.applyLocked()
	t.applyProfilesLocked()
	t.applyWidgetLocked()
	t.mu.Unlock()

	go func() {
		for {
			select {
			case <-open.ClickedCh:
				cb.Show()
			case <-t.toggle.ClickedCh:
				cb.Toggle()
			case <-quit.ClickedCh:
				cb.Quit()
				return
			}
		}
	}()
}

// SetState updates the icon, tooltip and toggle item; cheap to call repeatedly.
func (t *Tray) SetState(connected bool, tooltip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.connected == connected && t.tooltip == tooltip {
		return
	}
	t.connected, t.tooltip = connected, tooltip
	if t.ready {
		t.applyLocked()
	}
}

// SetWidget ticks or unticks the status-window item.
func (t *Tray) SetWidget(on bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.widgetOn = on
	if t.ready {
		t.applyWidgetLocked()
	}
}

func (t *Tray) applyWidgetLocked() {
	if t.widgetOn {
		t.widget.Check()
	} else {
		t.widget.Uncheck()
	}
}

// SetProfiles shows up to maxProfiles profile names; active gets a check mark.
func (t *Tray) SetProfiles(names []string, active int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.profNames, t.profAct = append([]string(nil), names...), active
	if t.ready {
		t.applyProfilesLocked()
	}
}

func (t *Tray) applyProfilesLocked() {
	if len(t.profNames) < 2 {
		t.profMenu.Hide() // nothing to switch between
	} else {
		t.profMenu.Show()
	}
	for i, it := range t.profItems {
		if i >= len(t.profNames) {
			it.Hide()
			continue
		}
		it.SetTitle(t.profNames[i])
		if i == t.profAct {
			it.Check()
		} else {
			it.Uncheck()
		}
		it.Show()
	}
}

func (t *Tray) applyLocked() {
	if t.connected {
		systray.SetIcon(t.iconOn)
		t.toggle.SetTitle(i18n.T("Отключить"))
	} else {
		systray.SetIcon(t.iconOff)
		t.toggle.SetTitle(i18n.T("Подключить"))
	}
	systray.SetTooltip(t.tooltip)
}

// Retitle shows the menu in the current language.
func (t *Tray) Retitle() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.ready {
		return
	}
	t.open.SetTitle(i18n.T("Открыть AWG Pilot"))
	t.widget.SetTitle(i18n.T("Окошко статуса"))
	t.widget.SetTooltip(i18n.T("Маленькое окно поверх других: зелёное — работает, красное — связь прервана"))
	t.profMenu.SetTitle(i18n.T("Профиль"))
	t.profMenu.SetTooltip(i18n.T("Переключить ключ или конфиг"))
	t.quit.SetTitle(i18n.T("Выход"))
	t.quit.SetTooltip(i18n.T("Отключить VPN и закрыть программу"))
	t.applyLocked()
}

func (t *Tray) Close() { systray.Quit() }

// circleICO renders a 32x32 filled circle and wraps the PNG in an ICO container.
func circleICO(c color.RGBA) []byte {
	const n = 32
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			dx, dy := float64(x)-15.5, float64(y)-15.5
			d := dx*dx + dy*dy
			switch {
			case d <= 13*13:
				img.Set(x, y, c)
			case d <= 15*15:
				img.Set(x, y, color.RGBA{255, 255, 255, 230}) // light ring keeps it visible on dark taskbars
			}
		}
	}
	var p bytes.Buffer
	png.Encode(&p, img)
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, [3]uint16{0, 1, 1}) // ICONDIR: reserved, type=icon, count
	entry := struct {
		W, H, Colors, Reserved uint8
		Planes, BPP            uint16
		Size, Offset           uint32
	}{n, n, 0, 0, 1, 32, uint32(p.Len()), 6 + 16}
	binary.Write(&b, binary.LittleEndian, entry)
	b.Write(p.Bytes())
	return b.Bytes()
}
