package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
)

// appDirName is the per-platform directory name used under the OS config/log roots.
func appDirName() string {
	if runtime.GOOS == "linux" {
		return "bitfocus-listener"
	}
	return "Bitfocus Listener"
}

// appConfigPath returns the OS-conventional config file location:
//
//	macOS   ~/Library/Application Support/Bitfocus Listener/config.json
//	Windows %APPDATA%\Bitfocus Listener\config.json
//	Linux   $XDG_CONFIG_HOME/bitfocus-listener/config.json
func appConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, appDirName())
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// appLogPath returns the OS-conventional log file location:
//
//	macOS   ~/Library/Logs/Bitfocus Listener/listener.log  (visible in Console.app)
//	Windows %LOCALAPPDATA%\Bitfocus Listener\Logs\listener.log
//	Linux   $XDG_STATE_HOME/bitfocus-listener/listener.log
func appLogPath() (string, error) {
	var dir string
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, "Library", "Logs", appDirName())
	case "windows":
		// UserCacheDir is %LocalAppData% on Windows.
		local, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(local, appDirName(), "Logs")
	default:
		state := os.Getenv("XDG_STATE_HOME")
		if state == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			state = filepath.Join(home, ".local", "state")
		}
		dir = filepath.Join(state, appDirName())
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "listener.log"), nil
}

// legacyHomePath returns the pre-1.1 dotfile location in $HOME, or "" if the
// home directory can't be resolved.
func legacyHomePath(name string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, name)
}

// migrateLegacyFile moves a pre-1.1 dotfile to its new location once. It is a
// no-op when the legacy file is absent or the new file already exists.
func migrateLegacyFile(legacy, current string) {
	if legacy == "" {
		return
	}
	if _, err := os.Stat(current); err == nil {
		return
	}
	if _, err := os.Stat(legacy); err != nil {
		return
	}
	if err := os.Rename(legacy, current); err == nil {
		log.Printf("Migrated %s to %s", legacy, current)
		return
	}
	// Rename fails across filesystems (e.g. $HOME and the log dir on separate
	// volumes); fall back to copy-then-remove.
	if err := copyFile(legacy, current); err != nil {
		log.Printf("Failed to migrate %s: %v", legacy, err)
		return
	}
	if err := os.Remove(legacy); err != nil {
		log.Printf("Migrated %s but could not remove the original: %v", legacy, err)
		return
	}
	log.Printf("Migrated %s to %s", legacy, current)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src) // #nosec G304 -- fixed application-controlled paths
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}
