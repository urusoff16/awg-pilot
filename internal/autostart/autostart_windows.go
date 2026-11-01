// Package autostart adds AWG Pilot to the current user's Startup list (the per-user Run key).
// The window needs no administrator rights: the tunnel itself runs in the AWG Pilot service.
package autostart

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows/registry"
)

const (
	runKey    = `Software\Microsoft\Windows\CurrentVersion\Run`
	valueName = "AWG Pilot"
)

// Enabled reports whether the Startup entry exists.
func Enabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(valueName)
	return err == nil
}

// Enable starts exe with args when the user signs in.
func Enable(exe, args string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("не удалось открыть список автозагрузки: %w", err)
	}
	defer k.Close()
	cmd := `"` + exe + `"`
	if args != "" {
		cmd += " " + args
	}
	if err := k.SetStringValue(valueName, cmd); err != nil {
		return fmt.Errorf("не удалось добавить в автозагрузку: %w", err)
	}
	return nil
}

// Disable removes the Startup entry; a missing entry is not an error.
func Disable() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return nil
	}
	defer k.Close()
	if err := k.DeleteValue(valueName); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("не удалось убрать из автозагрузки: %w", err)
	}
	return nil
}
