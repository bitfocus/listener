//go:build !windows && !darwin

package main

import "errors"

const loginItemBlockedHint = ""

func loginItemSupported() bool { return false }

func getLoginItemStatus() loginItemStatus { return loginItemOff }

func setLoginItem(bool) error { return errors.New("launch at login is not supported on this platform") }

func syncLoginItem() {}

func openLoginItemSettings() {}
