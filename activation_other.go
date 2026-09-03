//go:build !darwin

package main

import "fyne.io/fyne/v2"

func hideDockIcon() {}

// trayIconResource keeps the full-colour app icon in the tray on platforms
// without macOS template image handling.
func trayIconResource(appIcon fyne.Resource) fyne.Resource { return appIcon }
