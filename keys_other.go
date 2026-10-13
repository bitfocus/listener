//go:build !windows

package main

func virtualKeyCode(string) (uintptr, bool) { return 0, false }

func sendVirtualKey(uintptr, bool) {}
