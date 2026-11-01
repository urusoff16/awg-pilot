package i18n

import (
	"errors"
	"fmt"
	"testing"
)

func TestTranslate(t *testing.T) {
	SetLanguage("en")
	defer SetLanguage("ru")
	inner := errors.New("dial tcp: i/o timeout")
	wrapped := fmt.Errorf("туннель не поднят: %v", fmt.Errorf("не удалось запустить туннель: %w", inner))
	cases := map[string]string{
		"сервер сменён":                               "server changed",
		"выбран профиль «Мой VPN»":                    "profile \"Мой VPN\" selected",
		"12:34:56 служба: туннель опущен":             "12:34:56 service: tunnel down",
		"журнал трафика по программам включён":        "per-program traffic log on",
		"137 мс · ↓ 1.5 КБ/с · ↑ 1.2 КБ/с":            "137 ms · ↓ 1.5 KB/s · ↑ 1.2 KB/s",
		"Подписка заканчивается 05.11.2026 в 14:00":   "The subscription ends 05.11.2026 14:00",
		"сервер не ответил ни разу — переподключаюсь": "the server never answered: reconnecting",
		"not russian at all": "not russian at all",
	}
	cases[wrapped.Error()] = "tunnel not up: could not start the tunnel: dial tcp: i/o timeout"
	for in, want := range cases {
		if got := T(in); got != want {
			t.Errorf("T(%q)\n got %q\nwant %q", in, got, want)
		}
	}
	SetLanguage("ru")
	if got := T("сервер сменён"); got != "сервер сменён" {
		t.Errorf("Russian changed: %q", got)
	}
}

func TestResolve(t *testing.T) {
	for _, c := range [][3]string{{"", "ru-RU", "ru"}, {"", "en-US", "en"}, {"", "uk", "en"}, {"ru", "en", "ru"}, {"en", "ru", "en"}} {
		if got := Resolve(c[0], c[1]); got != c[2] {
			t.Errorf("Resolve(%q, %q) = %q", c[0], c[1], got)
		}
	}
}
