//go:build !windows

package i18n

// System is unknown here: Android passes the system language from Java instead.
func System() string { return "" }
