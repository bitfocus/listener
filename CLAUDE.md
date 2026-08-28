# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Bitfocus Listener is a Go application that acts as a remote control server, allowing keyboard, mouse, and system actions via WebSockets. It uses a Fyne GUI for configuration and is developed by Bitfocus AS. Most application logic lives in `main.go`, with thin platform helpers in `*_darwin.go` / `*_windows.go` / `*_other.go`.

## Build & Run Commands

```bash
# Development: run locally (updates version.go, then runs)
./tools/dev.sh

# Regenerate version.go only (no build/run)
./tools/dev-version.sh

# Run directly (requires version.go to exist)
go run .

# Production build (platform: mac|windows, arch: X64|ARM64)
./tools/build.sh mac ARM64
./tools/build.sh windows X64

# macOS app bundle with signing/notarization
./tools/make-mac-app.sh

# Install dependencies
go get ./...
```

There are no tests in this project.

## Architecture

**Core application** — primarily `main.go`, with platform files for Dock policy, Windows console hiding, and Darwin mouse:

1. **Configuration**: `Config` struct, load/save from the OS config dir (JSON). Config and audit-log paths come from `paths.go` (`appConfigPath` / `appLogPath`); the old `~/.bitfocus_listener.*` dotfiles are migrated on first run.
2. **System Info**: CPU/memory/process metrics via gopsutil
3. **WebSocket & Commands**: Auth, serialized writes, subscriptions, command handling
4. **Key Access Control**: Category-based and individual key whitelisting
5. **HTTP Server**: Start/stop/restart on interface:port with `/ws` endpoint (mutex-guarded)
6. **Network Interfaces**: Discovery for GUI binding dropdown
7. **GUI / tray**: Fyne settings window; system tray / menu bar with hide-on-close

**Key dependencies:**
- `fyne.io/fyne/v2` — cross-platform GUI + system tray
- `github.com/go-vgo/robotgo` — keyboard/mouse on Windows/Linux (not used for Darwin keyboard/mouse hot paths)
- `github.com/gorilla/websocket` — WebSocket server
- `github.com/shirou/gopsutil` — system metrics

## Version Management

- Source of truth: `version.json` (e.g., `{"version": "1.0.9"}`)
- `version.go` is **auto-generated** by build/dev tools — do not edit manually
- Build version format: `MAJOR.MINOR.PATCH+BUILD-BRANCH-HASH`
- Production tags must match `version.json` (validated in CI)

## WebSocket Protocol

- Auth uses MD5 challenge-response: server sends salt, client responds with `MD5(salt + password)`
- Auth is rate-limited per client IP (reconnect-safe)
- Command types: `keyPress`, `keyCombinationPress`, `keyDown`, `keyUp`, `keyString`, `shellRun`, `fileOpen`, `mousePositionSet`, `mousePositionGet`, `mouseClick`, `osxKeyPressProcess`, `osxAppleScript`, `subscribe` / `unsubscribe`
- All commands require authentication first; each command type can be individually enabled/disabled via `AllowedCommands`
- Key access has two modes: `full_access` (all keys) or `restricted` (only whitelisted categories/keys)
- Password rotate disconnects all active clients

## Platform-Specific Behavior

- macOS: System Events via `osascript` for keyboard; CoreGraphics for mouse and NX media keys (volume + transport); `open` for files; tray/menu bar + `LSUIElement` / accessory activation policy
- Windows: `cmd.exe` for shell (hidden console), `rundll32` FileProtocolHandler for files, system tray; requires syso resource generation when packaging
- Linux: `sh` for shell, `xdg-open` for files (builds not currently supported)
- CGO is required (`CGO_ENABLED=1`) due to Fyne and platform native helpers

## CI/CD

Follows the same shape as the other Bitfocus open source repos (see `bitfocus/companion`):

- **lint.yaml**: `[push, pull_request]` on `ubuntu-latest` — gofmt + `go vet`
- **build.yaml**: `[push]` — builds macOS ARM64/X64 on GitHub-hosted `macos-26` and Windows X64 on `windows-latest`
- Signing is opportunistic: macOS signs only when `CSC_LINK` is present, Windows signs only on the self-hosted `codecert` runner (selected via `runs-on` when the ref is a tag or the commit message contains `[build-signed]`)
- Upload steps are guarded by `github.repository_owner == 'bitfocus'`, so forks build but never publish
- Beta = push to `main` (`S3_*` vars), stable = `v*` tag (`RELEASE_S3_*` vars), both via `bitfocus/actions/upload-and-notify-for-branch@main`
- `tools/check-tag-version.sh` enforces that a `v*` tag matches `version.json`

## Licensing

- MIT (`LICENSE`), copyright Bitfocus AS. Third-party components in `THIRD-PARTY-NOTICES.md`
- `tools/build.sh` / `tools/make-mac-app.sh` ship `LICENSE` + `THIRD-PARTY-NOTICES.md` into the bundles
- Public-facing docs: `README.md`, `CONTRIBUTING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`
- Do not reintroduce internal-only or proprietary license language
