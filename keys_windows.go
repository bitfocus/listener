//go:build windows

package main

import (
	"strings"
	"syscall"
)

var procKeybdEvent = syscall.NewLazyDLL("user32.dll").NewProc("keybd_event")

const keyEventFKeyUp = 0x0002

// virtualKeys covers keys robotgo has no name for.
var virtualKeys = map[string]uintptr{
	"break":       0x13, // VK_PAUSE
	"pausebreak":  0x13,
	"pause_break": 0x13,
	"scroll":      0x91, // VK_SCROLL
	"scrolllock":  0x91,
	"scroll_lock": 0x91,
}

func virtualKeyCode(key string) (uintptr, bool) {
	vk, ok := virtualKeys[strings.ToLower(key)]
	return vk, ok
}

func sendVirtualKey(vk uintptr, down bool) {
	var flags uintptr
	if !down {
		flags = keyEventFKeyUp
	}
	procKeybdEvent.Call(vk, 0, flags, 0)
}
