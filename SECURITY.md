# Security Policy

## Reporting a vulnerability

Please report security issues privately to **security@bitfocus.io**, or via
GitHub's [private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)
on this repository. Do not open a public issue for an unpatched vulnerability.

Include a description, affected version (`version.json` / the version shown in
the GUI footer), platform, and reproduction steps. We aim to acknowledge within
five business days.

## Threat model — read this before deploying

Bitfocus Listener is, by design, a **remote-control server that executes input
and shell commands on the host machine**. An authenticated client can type
keystrokes, move and click the mouse, open files, and — when enabled — run
arbitrary shell commands and AppleScript. Treat access to the listener as
equivalent to interactive access to the machine.

### What the design does and does not give you

- **No transport encryption.** Traffic is plain `ws://`. Anyone on the network
  path can read commands and the challenge/response exchange. Run Listener only
  on trusted, segmented networks, or tunnel it (SSH, WireGuard, a TLS reverse
  proxy).
- **Authentication is MD5 challenge-response.** The server sends a random
  per-connection salt; the client replies with `MD5(salt + password)`. This
  keeps the password off the wire, but MD5 is not a modern primitive and the
  scheme provides no forward secrecy and no server authentication. It exists for
  compatibility with Bitfocus Companion modules.
- **Failed authentication is rate-limited per source IP** (5 attempts, 2 s
  cooldown, 5 min lockout) and survives reconnects.
- **Any origin may connect.** The WebSocket upgrade does not check `Origin`, so
  a web page in a browser on the same machine (or one that can resolve to it)
  can open a connection. It still cannot act without the password, but it can
  reach the auth endpoint. Bind to a specific interface and firewall the port.
- **Default bind is `0.0.0.0`.** The listener is reachable from the whole
  network unless you change the interface in the GUI.
- **The password is stored in cleartext** in `~/.bitfocus_listener.config`
  (mode `0600`) because the challenge-response scheme needs the original value.

### Reducing exposure

1. Bind to `127.0.0.1` if only local clients need access.
2. In **Security → Allowed Remote Actions**, disable everything you do not use.
   `shellRun` and `osxAppleScript` execute arbitrary operator-supplied code, and
   `fileOpen` can launch any program or script by path. They are the
   highest-value targets and are off on new installs; configs from earlier
   versions keep their existing settings.
3. Leave `fileWrite` off unless you need it. When it is on, it can only write
   plain-text data files (`.txt`, `.csv`, `.json`, `.srt`, …) inside the folders listed under
   **Security → Allowed write file paths**. Those folders can only be added in the
   GUI. The GUI refuses your whole home folder, drive roots, system folders,
   the Listener's own config and log folders, and autostart locations.
4. In **Security → Key Access Control**, switch to *Restricted* and allow only
   the keys you need.
5. Rotate the password (**Generate New**) if it may have been exposed; this
   disconnects all connected clients.
6. Review `~/.bitfocus_listener.log` — every command, auth attempt, and settings
   change is recorded with source IP.

### Out of scope

- The ability of an *authenticated* client to run commands that are explicitly
  enabled in the configuration. That is the product.
- Attacks requiring an attacker who already has local code execution as the same
  user (they can read the config file directly).
