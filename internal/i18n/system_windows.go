package i18n

import "golang.org/x/sys/windows"

var procUILang = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetUserDefaultUILanguage")

// System is the Windows display language as a two-letter code ("ru" for Russian).
func System() string {
	id, _, _ := procUILang.Call()
	if id&0x3ff == 0x19 { // LANG_RUSSIAN
		return "ru"
	}
	return "en"
}
