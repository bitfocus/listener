# Bitfocus Listener

Bitfocus Listener is a small remote-control server. It exposes a WebSocket
endpoint that lets an authenticated client simulate keyboard input, move and
click the mouse, open files, run shell commands, and stream basic system
metrics. It is written in Go with a [Fyne](https://fyne.io/) GUI for
configuration, and runs in the system tray / menu bar.

It was built by [Bitfocus AS](https://bitfocus.io) to remote control machines from
[Bitfocus Buttons](https://bitfocus.io/buttons) modules and other automation, and is
released under the MIT License.

> [!WARNING]
> Listener grants remote control of the machine it runs on. With `shellRun` or
> `osxAppleScript` enabled, an authenticated client can execute arbitrary code.
> Traffic is unencrypted and authentication is MD5 challenge-response. Read
> [SECURITY.md](SECURITY.md) before exposing it to any network you do not fully
> control.

---

## Installing

Download a build from the [releases page](https://github.com/bitfocus/listener/releases),
or build from source (see below).

On macOS, grant the app **Accessibility** permission (and **Automation** for
System Events) in System Settings → Privacy & Security. Keyboard and mouse
control will silently do nothing without it. The About tab has a button that
opens the right pane.

---

## Configuration

Everything is configured from the GUI and stored in
`~/.bitfocus_listener.config` (JSON, mode `0600`).

- **Connection** — bind interface, port (default `12001`), and the password. The
  password is generated on first run; **Generate New** rotates it and
  disconnects every connected client.
- **Security → Allowed Remote Actions** — enable or disable each command type
  individually.
- **Security → Key Access Control** — *Full Access* (any key) or *Restricted*,
  where you allow specific key categories and individual keys.
- **Activity Log** — a live view of the audit log, also written to
  `~/.bitfocus_listener.log` (rotated at 5 MB). Every connection, auth attempt,
  executed command, and settings change is recorded with source IP.

Closing the settings window hides it; the server keeps running. Use the tray /
menu bar icon to reopen it or to quit.

---

## Protocol

Connect to `ws://<host>:<port>/ws`. All messages are JSON objects with a `type`
field.

### Authentication

The server sends a challenge immediately on connect:

```json
{ "type": "authChallenge", "salt": "1a2b3c4d5e6f7a8b" }
```

Reply with the hex MD5 of the salt concatenated with the configured password:

```json
{ "type": "auth", "password": "<MD5(salt + password)>" }
```

The server answers with `{"type":"authResponse","status":"authenticated"}` or
`{"type":"authResponse","status":"failed"}`. Every command other than `auth` is
ignored until authentication succeeds. Failed attempts are rate-limited per
source IP (5 attempts, then a 5 minute lockout).

### Commands

| Type | Fields | Notes |
| --- | --- | --- |
| `keyPress` | `key` | Single key tap |
| `keyCombinationPress` | `key`, `modifiers[]` | e.g. `key: "t"`, `modifiers: ["cmd"]` |
| `keyDown` / `keyUp` | `key` | Hold and release |
| `keyString` | `msg` | Type a literal string |
| `mousePositionSet` | `x`, `y` | Coordinates as strings |
| `mousePositionGet` | — | Replies `mousePositionGetResponse` |
| `mouseClick` | `button`, `double` | `button`: `left`, `right`, `center`; `double`: `"true"` |
| `shellRun` | `shell` | Arbitrary shell command, 60 s timeout |
| `fileOpen` | `path` | Opens with the system handler |
| `osxKeyPressProcess` | `processName`, `key`, `modifiers[]` | macOS: activate an app, then send the key |
| `osxAppleScript` | `msg` | macOS: arbitrary AppleScript |
| `subscribe` / `unsubscribe` | `name` | `mousePosition` (1 s) or `sysInfo` (5 s) |

Every command type can be disabled individually in the GUI. A disabled command
returns `{"type":"error","message":"Command <type> is disabled on the server"}`.

Legacy aliases (`subscribeSysInfo`, `unsubscribeMousePosition`, …) are still
accepted and normalized.

### Key names

Use these exact lowercase strings in `key`, `modifiers`, and the key-access
configuration:

| Category | Names |
| --- | --- |
| Letters / digits | `a`–`z`, `0`–`9` |
| Navigation | `up`, `down`, `left`, `right`, `home`, `end`, `pageup`, `pagedown`, `tab`, `backspace`, `delete`, `insert` |
| Function | `f1`–`f24` (`f21`–`f24` on Windows/Linux only) |
| System | `esc` / `escape`, `enter` / `return`, `space`, `capslock`, `menu` (Windows/Linux only), `print`, `scroll`, `break` |
| Numpad | `num0`–`num9`, `num.`, `num+`, `num-`, `num*`, `num/`, `num_enter`, `num_equal`, `num_lock` (Clear on macOS) |
| Modifiers | `ctrl` / `control`, `alt` / `option`, `shift`, `cmd` / `command` / `win` / `meta` |
| Media | `volumeup`, `volumedown`, `mute`, `play`, `pause`, `next`, `previous` (`stop` on Windows/Linux only) |

Examples:

```json
{ "type": "keyPress", "key": "volumeup" }
{ "type": "keyCombinationPress", "key": "t", "modifiers": ["cmd"] }
{ "type": "keyString", "msg": "Hello world" }
```

### Subscription payloads

```json
{ "type": "mousePositionUpdate", "x": 100, "y": 200 }
{ "type": "sysInfoUpdate", "cpu": 12.5, "maxCPU": 48.0, "mem": 8589934592, "maxMem": 17179869184, "processes": 412 }
```

---

## Building from source

Requires Go (version pinned in `go.mod`) and a CGO toolchain — `CGO_ENABLED=1`
is mandatory, since Fyne and the platform helpers are CGO-based.

```bash
go mod download
./tools/dev.sh                 # regenerates version.go, then runs the app
```

Or, once `version.go` exists:

```bash
go run .
```

Packaged builds:

```bash
./tools/build.sh mac ARM64        # platform: mac|windows, arch: X64|ARM64
./tools/build.sh windows X64
./tools/make-mac-app.sh arm64     # macOS .app bundle + dmg
```

Signing is opt-in and driven entirely by the environment: `make-mac-app.sh`
signs only when `CSC_LINK` is set and notarizes only when `APPLE_ID`,
`APPLE_TEAM_ID` and `APPLE_APP_SPECIFIC_PASSWORD` are also set. Without them you
get a perfectly usable unsigned build, which is what CI produces for forks and
pull requests.

`version.go` is generated by the tooling and is gitignored — do not edit it by
hand. `version.json` holds the version number, and production tags must match it.

---

## Platform notes

- **macOS** — keyboard input goes through System Events via `osascript`, and
  media keys are posted in-process as NX aux-control events via CoreGraphics.
  robotgo's CGO keyboard path is deliberately avoided: it crashes with SIGTRAP
  when called from WebSocket goroutines. The app uses `LSUIElement` and sets the
  accessory activation policy so it stays out of the Dock.
- **Windows** — shell commands run through `cmd.exe` with the console window
  hidden; files open via `rundll32 url.dll,FileProtocolHandler`. Builds bundle
  a Mesa3D `opengl32.dll` for machines without a usable OpenGL driver.
- **Linux** — the code paths exist (`sh`, `xdg-open`, robotgo), but builds are
  not currently produced or tested.

---

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Security reports go to
domains@bitfocus.io — see [SECURITY.md](SECURITY.md).

## License

MIT — see [LICENSE](LICENSE). Third-party components and their licenses are
listed in [THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md).
