//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa
#import <Cocoa/Cocoa.h>

void setAccessoryActivationPolicy(void) {
	[NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
}
*/
import "C"

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// trayIconResource returns the menu bar glyph. Fyne only marks a systray icon
// as a macOS template image when the resource is a *theme.ThemedResource, so
// wrap the monochrome PNG in one; macOS then tints it for the current menu bar
// appearance instead of drawing our full-colour app icon.
func trayIconResource(fyne.Resource) fyne.Resource {
	return theme.NewThemedResource(fyne.NewStaticResource("tray_icon.png", trayIconPNG))
}

// hideDockIcon runs the app as an accessory (menu-bar) process so it does not
// appear in the Dock. Needed because GLFW/Fyne can reset activation policy even
// when Info.plist has LSUIElement=true.
func hideDockIcon() {
	C.setAccessoryActivationPolicy()
}
