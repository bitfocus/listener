//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"

	"golang.org/x/sys/windows/registry"
)

const (
	loginItemName = "Bitfocus Listener"
	runKeyPath    = `Software\Microsoft\Windows\CurrentVersion\Run`
	// Settings > Apps > Startup and Task Manager record their on/off switch here,
	// separately from the Run value itself.
	startupApprovedKeyPath = `Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\Run`
)

const loginItemBlockedHint = "Turned off in Windows Settings → Apps → Startup."

func loginItemSupported() bool { return true }

func loginItemCommand() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return `"` + exe + `"`, nil
}

func getLoginItemStatus() loginItemStatus {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return loginItemOff
	}
	defer k.Close()
	if _, _, err := k.GetStringValue(loginItemName); err != nil {
		return loginItemOff
	}
	if startupApprovalDisabled() {
		return loginItemBlocked
	}
	return loginItemOn
}

// startupApprovalDisabled reports whether the user switched the entry off in
// Windows. The value's first byte is odd when disabled (0x03, 0x07).
func startupApprovalDisabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, startupApprovedKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	data, _, err := k.GetBinaryValue(loginItemName)
	return err == nil && len(data) > 0 && data[0]&1 == 1
}

func setLoginItem(on bool) error {
	// A tick in Listener is the user's explicit choice, so it also clears an
	// earlier "off" from Windows Settings; unticking leaves no entry behind.
	if err := deleteRegistryValue(startupApprovedKeyPath, loginItemName); err != nil {
		return err
	}
	if !on {
		return deleteRegistryValue(runKeyPath, loginItemName)
	}
	command, err := loginItemCommand()
	if err != nil {
		return err
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(loginItemName, command)
}

func deleteRegistryValue(path, name string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

// syncLoginItem points an existing entry at this executable. The Windows build
// is a folder people move, which would otherwise leave a dead entry.
func syncLoginItem() {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	current, _, err := k.GetStringValue(loginItemName)
	if err != nil {
		return
	}
	command, err := loginItemCommand()
	if err != nil || command == current {
		return
	}
	if err := k.SetStringValue(loginItemName, command); err == nil {
		auditLog("system", "LOGIN_ITEM_UPDATED", "path="+command)
	}
}

func openLoginItemSettings() {
	_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", "ms-settings:startupapps").Start()
}
