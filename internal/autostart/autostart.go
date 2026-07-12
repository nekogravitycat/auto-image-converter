// Package autostart manages whether the application launches automatically when
// the user logs in, via the per-user Windows "Run" registry key. This replaces
// the manual "put a shortcut in shell:startup" step: the UI's autostart checkbox
// toggles it directly.
package autostart

import (
	"errors"

	"golang.org/x/sys/windows/registry"
)

// runKeyPath is the standard per-user auto-run key. Values placed here are
// launched at logon for the current user only (no admin rights required).
const runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`

// valueName identifies this application's entry under the Run key.
const valueName = "AutoImageConverter"

// Enabled reports whether the auto-run entry is present.
func Enabled() (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer k.Close()

	if _, _, err := k.GetStringValue(valueName); err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Enable adds (or updates) the auto-run entry so the executable at exePath
// launches at logon. The path is quoted so a location containing spaces still
// launches correctly.
func Enable(exePath string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(valueName, `"`+exePath+`"`)
}

// Disable removes the auto-run entry. It is not an error if the entry is already
// absent.
func Disable() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(valueName); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

// Set enables or disables autostart to match `enabled`, using exePath when
// enabling.
func Set(enabled bool, exePath string) error {
	if enabled {
		return Enable(exePath)
	}
	return Disable()
}
