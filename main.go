package main

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/subtle"
	_ "embed" // for //go:embed icon.png (app icon)
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/color"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/go-vgo/robotgo"
	"github.com/gorilla/websocket"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/mem"
	"github.com/shirou/gopsutil/process"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// iconPNG is the application icon, embedded from icon.png at the repo root.
// Used for the in-app window/taskbar icon via myApp.SetIcon. The packaged
// bundle/exe icons (.icns/.ico) are generated separately by tools/gen-icons.sh.
//
//go:embed icon.png
var iconPNG []byte

// trayIconPNG is the monochrome menu-bar/tray glyph. It is a macOS template
// image (shape carried entirely in the alpha channel) so the system can tint
// it for light/dark menu bars.
//
//go:embed tray_icon.png
var trayIconPNG []byte

// -------------------------
// Configuration Handling
// -------------------------

type Config struct {
	Password        string          `json:"password"`
	Port            string          `json:"port"`
	Interface       string          `json:"interface"`
	AllowedCommands map[string]bool `json:"allowed_commands"`
	// New fields for granular key control
	KeyControlMode string          `json:"key_control_mode"` // "full_access" or "restricted"
	AllowedKeys    map[string]bool `json:"allowed_keys"`     // Map of allowed keys
	KeyCategories  map[string]bool `json:"key_categories"`   // Map of allowed key categories
}

var config Config
var configMu sync.RWMutex // protects config reads/writes
var configPath string
var storedPassword string
var connectionStatus string = "Disconnected"
var activeConnections int
var connMu sync.Mutex // protects activeConnections and connection status UI widgets

// GUI status widgets / hooks (nil until startGUI runs).
var connectionStatusText *canvas.Text
var connectionDetailText *canvas.Text
var connectionStatusBg *canvas.Rectangle
var listenStatusText *canvas.Text
var listenDetailText *canvas.Text
var listenStatusBg *canvas.Rectangle
var refreshTrayMenu func()

var bindStatusMu sync.Mutex
var lastBindError error
var lastBindAddr string

// validAppleScriptName matches safe application/process names (alphanumeric, spaces, hyphens, dots).
var validAppleScriptName = regexp.MustCompile(`^[a-zA-Z0-9 .\-_]+$`)

// defaultCommands lists the command types that can be toggled.
var defaultCommands = []string{
	"keyPress", "keyCombinationPress",
	"keyDown", "keyUp", "osxKeyPressProcess", "osxAppleScript", "keyString", "shellRun",
	"fileOpen", "mousePositionSet", "mousePositionGet", "mouseClick",
	"subscribe",
}

// remove defaultCommands starting with osx from non-darwin systems
func init() {
	if runtime.GOOS != "darwin" {
		var newCommands []string
		for _, cmd := range defaultCommands {
			if len(cmd) < 3 || cmd[:3] != "osx" {
				newCommands = append(newCommands, cmd)
			}
		}
		defaultCommands = newCommands
	}
}

func loadConfig() error {
	path, err := appConfigPath()
	if err != nil {
		return err
	}
	configPath = path
	migrateLegacyFile(legacyHomePath(".bitfocus_listener.config"), configPath)
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		config = Config{
			Password:  generatePassword(),
			Port:      "12001",
			Interface: "0.0.0.0",
		}
		// Enable all default commands.
		config.AllowedCommands = make(map[string]bool)
		for _, cmd := range defaultCommands {
			config.AllowedCommands[cmd] = true
		}
		// Initialize key control with full access by default
		config.KeyControlMode = "full_access"
		config.AllowedKeys = make(map[string]bool)
		config.KeyCategories = make(map[string]bool)
		storedPassword = config.Password
		return saveConfig(&config)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return err
	}
	// In case AllowedCommands is missing, set defaults.
	if config.AllowedCommands == nil {
		config.AllowedCommands = make(map[string]bool)
		for _, cmd := range defaultCommands {
			config.AllowedCommands[cmd] = true
		}
	}
	// Initialize key control fields if missing (for backward compatibility)
	if config.KeyControlMode == "" {
		config.KeyControlMode = "full_access"
	}
	if config.AllowedKeys == nil {
		config.AllowedKeys = make(map[string]bool)
	}
	if config.KeyCategories == nil {
		config.KeyCategories = make(map[string]bool)
	}
	storedPassword = config.Password
	return nil
}

// saveConfigLocked writes the config to disk. Caller must hold configMu.
func saveConfigLocked(cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, data, 0600)
}

// saveConfig acquires the lock and writes the config to disk.
func saveConfig(cfg *Config) error {
	configMu.Lock()
	defer configMu.Unlock()
	return saveConfigLocked(cfg)
}

// currentPassword returns the active password under configMu. storedPassword is
// written by the GUI's rotate action, so GUI and WebSocket readers must not
// touch it directly.
func currentPassword() string {
	configMu.RLock()
	defer configMu.RUnlock()
	return storedPassword
}

func generatePassword() string {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		log.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func generateSalt() string {
	b := make([]byte, 8)
	_, err := rand.Read(b)
	if err != nil {
		log.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func computeMD5(s string) string {
	h := md5.New()
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}

// -------------------------
// Audit Logger
// -------------------------

const (
	auditRingSize    = 500
	auditMaxFileSize = 5 * 1024 * 1024 // 5 MB
)

type AuditEntry struct {
	Time   time.Time
	IP     string
	Action string
	Detail string
}

func (e AuditEntry) String() string {
	return fmt.Sprintf("%s [%s] %s %s", e.Time.Format("2006-01-02 15:04:05"), e.IP, e.Action, e.Detail)
}

// AuditLogger holds an in-memory ring buffer and writes to a log file.
type AuditLogger struct {
	mu       sync.Mutex
	entries  []AuditEntry
	seq      uint64
	file     *os.File
	filePath string
	fileSize int64
}

var audit *AuditLogger

func initAuditLogger() error {
	logPath, err := appLogPath()
	if err != nil {
		return err
	}
	migrateLegacyFile(legacyHomePath(".bitfocus_listener.log"), logPath)
	migrateLegacyFile(legacyHomePath(".bitfocus_listener.log.1"), logPath+".1")

	a := &AuditLogger{
		entries:  make([]AuditEntry, 0, auditRingSize),
		filePath: logPath,
	}

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	a.file = f
	a.fileSize = info.Size()
	audit = a
	return nil
}

func (a *AuditLogger) rotateIfNeeded() {
	if a.file == nil || a.fileSize < auditMaxFileSize {
		return
	}
	old := a.file
	oldPath := a.filePath + ".1"
	os.Remove(oldPath)
	if err := os.Rename(a.filePath, oldPath); err != nil {
		log.Printf("Failed to rotate audit log (rename): %v", err)
		return
	}
	// a.filePath is a fixed, application-controlled path (see appLogPath),
	// never user input, so reopening it cannot be abused for file inclusion.
	// nosec
	f, err := os.OpenFile(a.filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		log.Printf("Failed to rotate audit log: %v", err)
		a.file = nil
		old.Close()
		return
	}
	a.file = f
	a.fileSize = 0
	old.Close()
}

func (a *AuditLogger) Log(ip, action, detail string) {
	entry := AuditEntry{
		Time:   time.Now(),
		IP:     ip,
		Action: action,
		Detail: detail,
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	// Ring buffer: keep last auditRingSize entries
	if len(a.entries) >= auditRingSize {
		a.entries = a.entries[1:]
	}
	a.entries = append(a.entries, entry)
	a.seq++

	// Write to file
	if a.file == nil {
		return
	}
	line := entry.String() + "\n"
	n, err := a.file.WriteString(line)
	if err != nil {
		log.Printf("Failed to write audit log: %v", err)
	} else {
		a.fileSize += int64(n)
		a.rotateIfNeeded()
	}
}

// Snapshot returns a copy of the ring buffer contents along with the total
// number of entries ever logged. Once the ring is full its length stops
// changing, so the counter is the only reliable way for viewers to tell that
// the contents moved on.
func (a *AuditLogger) Snapshot() ([]AuditEntry, uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := make([]AuditEntry, len(a.entries))
	copy(result, a.entries)
	return result, a.seq
}

// auditLog is the convenience function used throughout the codebase.
func auditLog(ip, action, detail string) {
	if audit != nil {
		audit.Log(ip, action, detail)
	}
}

// truncateForLog limits a string for safe log output.
func truncateForLog(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return s
}

// sanitizeOpenTarget validates a user-supplied target for the fileOpen command.
// It rejects empty values, embedded control characters, and arguments that
// could be interpreted as command-line flags (leading "-"). Combined with
// invoking the opener without a shell, this prevents argument/command injection.
func sanitizeOpenTarget(target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", errors.New("empty path")
	}
	if strings.ContainsAny(target, "\x00\n\r") {
		return "", errors.New("path contains control characters")
	}
	if strings.HasPrefix(target, "-") {
		return "", errors.New("path may not start with '-'")
	}
	return target, nil
}

// -------------------------
// System Information (gopsutil)
// -------------------------

type SysInfo struct {
	CPU          float64
	MaxCPU       float64
	CPUCores     int
	Memory       float64
	MaxMemory    uint64
	ProcessCount int
}

func getSystemInfo() (SysInfo, error) {
	var info SysInfo
	// Aggregate CPU since last sample (or immediately after first call).
	cpuPercents, err := cpu.Percent(0, false)
	if err != nil || len(cpuPercents) == 0 {
		return info, err
	}
	info.CPU = cpuPercents[0]

	// Per-core CPU; MaxCPU is the hottest core.
	perCore, err := cpu.Percent(0, true)
	if err != nil || len(perCore) == 0 {
		return info, err
	}
	maxCPU := perCore[0]
	for _, p := range perCore[1:] {
		if p > maxCPU {
			maxCPU = p
		}
	}
	info.MaxCPU = maxCPU

	vmStat, err := mem.VirtualMemory()
	if err != nil {
		return info, err
	}
	info.Memory = float64(vmStat.Used)
	info.MaxMemory = vmStat.Total

	pids, err := process.Pids()
	if err != nil {
		return info, err
	}
	info.ProcessCount = len(pids)
	return info, nil
}

// -------------------------
// WebSocket & Command Handling
// -------------------------

type Command struct {
	Type        string   `json:"type"`
	Key         string   `json:"key,omitempty"`
	Modifiers   []string `json:"modifiers,omitempty"`
	Msg         string   `json:"msg,omitempty"`
	Shell       string   `json:"shell,omitempty"`
	Path        string   `json:"path,omitempty"`
	X           string   `json:"x,omitempty"`
	Y           string   `json:"y,omitempty"`
	Button      string   `json:"button,omitempty"`
	Double      string   `json:"double,omitempty"`
	ProcessName string   `json:"processName,omitempty"`
	Name        string   `json:"name,omitempty"` // For subscribe/unsubscribe events.
	Password    string   `json:"password,omitempty"`
}

type Client struct {
	Conn    *websocket.Conn
	writeMu sync.Mutex // serializes all WebSocket writes
	subMu   sync.Mutex // protects Subscriptions map
	// authenticated is written by this client's read goroutine and read from the
	// GUI goroutine (authenticatedClientIPs), so it must not be a plain bool.
	authenticated atomic.Bool
	Salt          string
	IP            string               // remote address for audit logging
	Subscriptions map[string]chan bool // key: subscription type (e.g., "mousePosition", "sysInfo")
}

func (c *Client) writeJSON(v interface{}) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.Conn.WriteJSON(v)
}

const maxAuthAttempts = 5
const authCooldown = 2 * time.Second
const authLockout = 5 * time.Minute
const shellTimeout = 60 * time.Second

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Active WebSocket clients (for password rotate / broadcast disconnect).
var (
	clientsMu sync.Mutex
	clients   = map[*Client]struct{}{}
)

func registerClient(c *Client) {
	clientsMu.Lock()
	clients[c] = struct{}{}
	clientsMu.Unlock()
}

func unregisterClient(c *Client) {
	clientsMu.Lock()
	delete(clients, c)
	clientsMu.Unlock()
}

func disconnectAllClients() {
	clientsMu.Lock()
	list := make([]*Client, 0, len(clients))
	for c := range clients {
		list = append(list, c)
	}
	clientsMu.Unlock()
	for _, c := range list {
		_ = c.Conn.Close()
	}
}

// IP-based auth rate limiting (survives reconnects).
type authBucket struct {
	attempts    int
	last        time.Time
	lockedUntil time.Time
}

var (
	authMu   sync.Mutex
	authByIP = map[string]*authBucket{}
)

func checkAuthRateLimit(ip string) (ok bool, message string) {
	authMu.Lock()
	defer authMu.Unlock()
	b := authByIP[ip]
	if b == nil {
		b = &authBucket{}
		authByIP[ip] = b
	}
	now := time.Now()
	if now.Before(b.lockedUntil) {
		return false, "Too many auth attempts"
	}
	if !b.last.IsZero() && now.Sub(b.last) < authCooldown {
		return false, "Too fast, slow down"
	}
	if b.attempts >= maxAuthAttempts {
		b.lockedUntil = now.Add(authLockout)
		b.attempts = 0
		return false, "Too many auth attempts"
	}
	b.last = now
	return true, ""
}

func recordAuthFailure(ip string) {
	authMu.Lock()
	defer authMu.Unlock()
	b := authByIP[ip]
	if b == nil {
		b = &authBucket{}
		authByIP[ip] = b
	}
	b.attempts++
	b.last = time.Now()
}

func clearAuthRateLimit(ip string) {
	authMu.Lock()
	delete(authByIP, ip)
	authMu.Unlock()
}

func logCommand(cmd Command) {
	switch cmd.Type {
	case "auth":
		log.Printf("Received command: type=auth")
	case "shellRun":
		log.Printf("Received command: type=shellRun shell=%q", truncateForLog(cmd.Shell, 80))
	case "osxAppleScript", "keyString":
		log.Printf("Received command: type=%s msg=%q", cmd.Type, truncateForLog(cmd.Msg, 80))
	default:
		log.Printf("Received command: type=%s key=%q name=%q", cmd.Type, cmd.Key, cmd.Name)
	}
}

func stopSubscription(client *Client, name string) {
	client.subMu.Lock()
	defer client.subMu.Unlock()
	if ch, ok := client.Subscriptions[name]; ok {
		delete(client.Subscriptions, name)
		close(ch)
	}
}

func toInterfaceSlice(s []string) []interface{} {
	r := make([]interface{}, len(s))
	for i, v := range s {
		r[i] = v
	}
	return r
}

// darwinKeyCodes maps named keys to macOS virtual key codes for System Events.
// robotgo's CGO keyboard path crashes with SIGTRAP on some macOS versions when
// called from non-main threads (WebSocket handlers), so we drive keys via osascript.
var darwinKeyCodes = map[string]int{
	"return": 36, "enter": 36,
	"tab": 48, "space": 49,
	"delete": 51, "backspace": 51,
	"escape": 53, "esc": 53,
	"cmd": 55, "command": 55, "win": 55, "meta": 55,
	"shift":    56,
	"capslock": 57, "caps_lock": 57,
	"option": 58, "alt": 58,
	"control": 59, "ctrl": 59,
	"right": 124, "left": 123, "down": 125, "up": 126,
	"f1": 122, "f2": 120, "f3": 99, "f4": 118,
	"f5": 96, "f6": 97, "f7": 98, "f8": 100,
	"f9": 101, "f10": 109, "f11": 103, "f12": 111,
	"home": 115, "end": 119, "pageup": 116, "pagedown": 121,
	"forwarddelete": 117, "fwd_delete": 117,
}

// robotgoKeyAliases maps Listener key names to robotgo's expected names.
var robotgoKeyAliases = map[string]string{
	"volumeup":   "audio_vol_up",
	"volumedown": "audio_vol_down",
	"mute":       "audio_mute",
	"play":       "audio_play",
	"pause":      "audio_pause",
	"stop":       "audio_stop",
	"next":       "audio_next",
	"previous":   "audio_prev",
}

// NX_KEYTYPE_* values from IOKit/hidsystem/ev_keymap.h
const (
	nxKeyTypeSoundUp   = 0
	nxKeyTypeSoundDown = 1
	nxKeyTypeMute      = 7
	nxKeyTypePlay      = 16
	nxKeyTypeNext      = 17
	nxKeyTypePrevious  = 18
)

// darwinNXMediaKeys maps Listener media key names to NX_KEYTYPE_* codes.
// play and pause both use NX_KEYTYPE_PLAY (play/pause toggle). stop has no NX equivalent on macOS.
// All entries are posted in-process via CoreGraphics so Listener's Accessibility grant applies.
var darwinNXMediaKeys = map[string]int{
	"volumeup":   nxKeyTypeSoundUp,
	"volumedown": nxKeyTypeSoundDown,
	"mute":       nxKeyTypeMute,
	"play":       nxKeyTypePlay,
	"pause":      nxKeyTypePlay,
	"next":       nxKeyTypeNext,
	"previous":   nxKeyTypePrevious,
}

func normalizeRobotgoKey(key string) string {
	if alias, ok := robotgoKeyAliases[strings.ToLower(key)]; ok {
		return alias
	}
	return key
}

func appleScriptString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

func darwinModifiersClause(modifiers []string) string {
	var modifierStrs []string
	for _, mod := range modifiers {
		switch strings.ToLower(mod) {
		case "alt", "option":
			modifierStrs = append(modifierStrs, "option down")
		case "ctrl", "control":
			modifierStrs = append(modifierStrs, "control down")
		case "shift":
			modifierStrs = append(modifierStrs, "shift down")
		case "cmd", "command", "win", "meta":
			modifierStrs = append(modifierStrs, "command down")
		}
	}
	if len(modifierStrs) == 0 {
		return ""
	}
	return " using {" + strings.Join(modifierStrs, ", ") + "}"
}

func runOsascript(script string) {
	if err := exec.Command("osascript", "-e", script).Run(); err != nil {
		log.Printf("osascript failed: %v", err)
	}
}

func darwinKeyTapScript(key string, modifiers []string) (string, bool) {
	if key == "" || strings.ContainsAny(key, "\"\\") {
		return "", false
	}
	mod := darwinModifiersClause(modifiers)
	runes := []rune(key)
	if len(runes) == 1 && unicode.IsPrint(runes[0]) {
		return fmt.Sprintf(`tell application "System Events" to keystroke %s%s`, appleScriptString(key), mod), true
	}
	code, ok := darwinKeyCodes[strings.ToLower(key)]
	if !ok {
		return "", false
	}
	return fmt.Sprintf(`tell application "System Events" to key code %d%s`, code, mod), true
}

func darwinKeyToggleScript(key string, down bool) (string, bool) {
	if key == "" || strings.ContainsAny(key, "\"\\") {
		return "", false
	}
	action := "key up"
	if down {
		action = "key down"
	}
	runes := []rune(key)
	if len(runes) == 1 && unicode.IsPrint(runes[0]) {
		return fmt.Sprintf(`tell application "System Events" to %s %s`, action, appleScriptString(key)), true
	}
	code, ok := darwinKeyCodes[strings.ToLower(key)]
	if !ok {
		return "", false
	}
	return fmt.Sprintf(`tell application "System Events" to %s %d`, action, code), true
}

func isDarwinMediaKey(key string) bool {
	if strings.EqualFold(key, "stop") {
		return true
	}
	_, ok := darwinNXMediaKeys[strings.ToLower(key)]
	return ok
}

// darwinMediaTap handles volume/media keys that cannot use System Events key codes.
// Posts NX aux-control events in-process via CoreGraphics so they inherit Listener's
// Accessibility TCC grant (posting via osascript often fails as a different process).
func darwinMediaTap(key string) {
	if strings.EqualFold(key, "stop") {
		log.Printf("Unsupported macOS media key (no system stop key): %q", key)
		return
	}
	nx, ok := darwinNXMediaKeys[strings.ToLower(key)]
	if !ok {
		return
	}
	postDarwinMediaKey(nx)
}

// tapKey sends a key press. On macOS this uses System Events / CoreGraphics media keys
// (not robotgo) to avoid CGO SIGTRAP crashes from background goroutines.
func tapKey(key string, modifiers ...string) {
	if runtime.GOOS == "darwin" {
		if len(modifiers) == 0 && isDarwinMediaKey(key) {
			darwinMediaTap(key)
			return
		}
		if script, ok := darwinKeyTapScript(key, modifiers); ok {
			runOsascript(script)
			return
		}
		log.Printf("Unsupported macOS key for AppleScript fallback: %q", key)
		return
	}
	robotgo.KeyTap(normalizeRobotgoKey(key), toInterfaceSlice(modifiers)...)
}

// toggleKey sends key down/up. On macOS this uses System Events (osascript).
// Media keys have no meaningful hold state; a down is treated as a tap and up is ignored.
func toggleKey(key string, down bool) {
	if runtime.GOOS == "darwin" {
		if isDarwinMediaKey(key) {
			if down {
				darwinMediaTap(key)
			}
			return
		}
		if script, ok := darwinKeyToggleScript(key, down); ok {
			runOsascript(script)
			return
		}
		log.Printf("Unsupported macOS key for AppleScript fallback: %q", key)
		return
	}
	key = normalizeRobotgoKey(key)
	if down {
		robotgo.KeyDown(key)
	} else {
		robotgo.KeyUp(key)
	}
}

// typeString types a string. On macOS this uses System Events keystroke via
// osascript to avoid robotgo CGO crashes from WebSocket goroutines.
func typeString(msg string) {
	if runtime.GOOS == "darwin" {
		script := fmt.Sprintf(`tell application "System Events" to keystroke %s`, appleScriptString(msg))
		runOsascript(script)
		return
	}
	robotgo.TypeStr(msg)
}

// normalizeLegacyCommand rewrites legacy/alias command types/names to the
// current unified API so older clients continue to work without server errors.
func normalizeLegacyCommand(cmd *Command) {
	switch cmd.Type {
	case "subscribeSysInfo":
		cmd.Type = "subscribe"
		cmd.Name = "sysInfo"
	case "subscribeMousePosition":
		cmd.Type = "subscribe"
		cmd.Name = "mousePosition"
	case "unsubscribeSysInfo":
		cmd.Type = "unsubscribe"
		cmd.Name = "sysInfo"
	case "unsubscribeMousePosition":
		cmd.Type = "unsubscribe"
		cmd.Name = "mousePosition"
	}
}

// Updated handleCommand with unified subscription handling.
func handleCommand(client *Client, cmd Command) {
	logCommand(cmd)
	// Allow auth command regardless.
	if cmd.Type != "auth" && cmd.Type != "unsubscribe" {
		configMu.RLock()
		allowed, exists := config.AllowedCommands[cmd.Type]
		configMu.RUnlock()
		if !exists || !allowed {
			log.Printf("Command %s is disabled", cmd.Type)
			auditLog(client.IP, "BLOCKED", fmt.Sprintf("command %s is disabled", cmd.Type))
			_ = client.writeJSON(map[string]interface{}{
				"type":    "error",
				"message": "Command " + cmd.Type + " is disabled on the server",
			})
			return
		}
	}
	if !client.authenticated.Load() {
		if cmd.Type == "auth" {
			if ok, msg := checkAuthRateLimit(client.IP); !ok {
				log.Printf("Auth rate limited for %s: %s", client.IP, msg)
				auditLog(client.IP, "AUTH_RATE_LIMITED", msg)
				_ = client.writeJSON(map[string]interface{}{
					"type":    "authResponse",
					"status":  "failed",
					"message": msg,
				})
				return
			}

			expected := computeMD5(client.Salt + currentPassword())
			// Constant-time compare so response timing cannot leak how much of
			// the digest matched.
			if subtle.ConstantTimeCompare([]byte(cmd.Password), []byte(expected)) == 1 {
				client.authenticated.Store(true)
				client.subMu.Lock()
				client.Subscriptions = make(map[string]chan bool)
				client.subMu.Unlock()
				clearAuthRateLimit(client.IP)
				log.Println("Client authenticated successfully")
				auditLog(client.IP, "AUTH_SUCCESS", "")
				_ = client.writeJSON(map[string]interface{}{
					"type":   "authResponse",
					"status": "authenticated",
				})
				incrementConnections()
			} else {
				recordAuthFailure(client.IP)
				log.Printf("Authentication failed for %s", client.IP)
				auditLog(client.IP, "AUTH_FAILED", "")
				_ = client.writeJSON(map[string]interface{}{
					"type":   "authResponse",
					"status": "failed",
				})
			}
		} else {
			log.Println("Client not authenticated. Ignoring command.")
		}
		return
	}

	switch cmd.Type {

	case "subscribe":
		client.subMu.Lock()
		if client.Subscriptions == nil {
			client.Subscriptions = make(map[string]chan bool)
		}
		client.subMu.Unlock()
		switch cmd.Name {
		case "mousePosition":
			client.subMu.Lock()
			if _, exists := client.Subscriptions["mousePosition"]; exists {
				client.subMu.Unlock()
				log.Println("Already subscribed to mousePosition updates")
				return
			}
			ch := make(chan bool)
			client.Subscriptions["mousePosition"] = ch
			client.subMu.Unlock()
			go func(stop chan bool) {
				ticker := time.NewTicker(1 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						x, y := getMousePos()
						response := map[string]interface{}{
							"type": "mousePositionUpdate",
							"x":    x,
							"y":    y,
						}
						if err := client.writeJSON(response); err != nil {
							log.Printf("Error sending mouse position update: %v", err)
							stopSubscription(client, "mousePosition")
							return
						}
					case <-stop:
						log.Println("MousePosition subscription stopped")
						return
					}
				}
			}(ch)
			auditLog(client.IP, "subscribe", "mousePosition")
			log.Println("Subscribed to mousePosition updates")
		case "sysInfo":
			client.subMu.Lock()
			if _, exists := client.Subscriptions["sysInfo"]; exists {
				client.subMu.Unlock()
				log.Println("Already subscribed to sysInfo updates")
				return
			}
			ch := make(chan bool)
			client.Subscriptions["sysInfo"] = ch
			client.subMu.Unlock()
			go func(stop chan bool) {
				ticker := time.NewTicker(5 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						if !sendSysInfo(client) {
							stopSubscription(client, "sysInfo")
							return
						}
					case <-stop:
						log.Println("sysInfo subscription stopped")
						return
					}
				}
			}(ch)
			auditLog(client.IP, "subscribe", "sysInfo")
			log.Println("Subscribed to sysInfo updates")
		default:
			log.Printf("Unknown subscription type: %s", cmd.Name)
			_ = client.writeJSON(map[string]interface{}{
				"type":    "error",
				"message": "Unknown subscription type: " + cmd.Name,
			})
		}

	case "unsubscribe":
		switch cmd.Name {
		case "mousePosition", "sysInfo":
			client.subMu.Lock()
			_, ok := client.Subscriptions[cmd.Name]
			client.subMu.Unlock()
			if ok {
				stopSubscription(client, cmd.Name)
				auditLog(client.IP, "unsubscribe", cmd.Name)
				log.Printf("Unsubscribed from %s updates", cmd.Name)
			}
		default:
			log.Printf("Unknown subscription type: %s", cmd.Name)
			_ = client.writeJSON(map[string]interface{}{
				"type":    "error",
				"message": "Unknown subscription type: " + cmd.Name,
			})
		}

	case "keyPress":
		// First check if the key is allowed
		if !isKeyAllowed(cmd.Key) {
			log.Printf("Key %s is not allowed", cmd.Key)
			auditLog(client.IP, "KEY_BLOCKED", fmt.Sprintf("keyPress key=%s", cmd.Key))
			client.writeJSON(map[string]interface{}{
				"type":    "error",
				"message": "Key " + cmd.Key + " is not allowed",
			})
			return
		}

		auditLog(client.IP, "keyPress", fmt.Sprintf("key=%s", cmd.Key))
		tapKey(cmd.Key)
		log.Printf("Executed key tap: %v", cmd)

	case "keyCombinationPress":
		// Check if the main key is allowed
		if !isKeyAllowed(cmd.Key) {
			log.Printf("Key %s is not allowed", cmd.Key)
			auditLog(client.IP, "KEY_BLOCKED", fmt.Sprintf("keyCombinationPress key=%s", cmd.Key))
			client.writeJSON(map[string]interface{}{
				"type":    "error",
				"message": "Key " + cmd.Key + " is not allowed",
			})
			return
		}
		// Check if all modifiers are allowed
		for _, modifier := range cmd.Modifiers {
			if !isKeyAllowed(modifier) {
				log.Printf("Modifier %s is not allowed", modifier)
				auditLog(client.IP, "KEY_BLOCKED", fmt.Sprintf("keyCombinationPress modifier=%s", modifier))
				client.writeJSON(map[string]interface{}{
					"type":    "error",
					"message": "Modifier " + modifier + " is not allowed",
				})
				return
			}
		}
		auditLog(client.IP, "keyCombinationPress", fmt.Sprintf("key=%s modifiers=%v", cmd.Key, cmd.Modifiers))
		tapKey(cmd.Key, cmd.Modifiers...)
		log.Printf("Executed key tap with modifiers: %s + %v", cmd.Key, cmd.Modifiers)

	case "keyDown":
		if !isKeyAllowed(cmd.Key) {
			log.Printf("Key %s is not allowed", cmd.Key)
			auditLog(client.IP, "KEY_BLOCKED", fmt.Sprintf("keyDown key=%s", cmd.Key))
			client.writeJSON(map[string]interface{}{
				"type":    "error",
				"message": "Key " + cmd.Key + " is not allowed",
			})
			return
		}
		auditLog(client.IP, "keyDown", fmt.Sprintf("key=%s", cmd.Key))
		toggleKey(cmd.Key, true)
		log.Printf("Key down: %s", cmd.Key)

	case "keyUp":
		if !isKeyAllowed(cmd.Key) {
			log.Printf("Key %s is not allowed", cmd.Key)
			auditLog(client.IP, "KEY_BLOCKED", fmt.Sprintf("keyUp key=%s", cmd.Key))
			client.writeJSON(map[string]interface{}{
				"type":    "error",
				"message": "Key " + cmd.Key + " is not allowed",
			})
			return
		}
		auditLog(client.IP, "keyUp", fmt.Sprintf("key=%s", cmd.Key))
		toggleKey(cmd.Key, false)
		log.Printf("Key up: %s", cmd.Key)

	case "osxKeyPressProcess":
		if runtime.GOOS == "darwin" {
			// Validate processName to prevent AppleScript injection
			if !validAppleScriptName.MatchString(cmd.ProcessName) {
				log.Printf("Invalid process name rejected: %q", cmd.ProcessName)
				client.writeJSON(map[string]interface{}{
					"type":    "error",
					"message": "Invalid process name",
				})
				return
			}
			// Check if the main key is allowed
			if !isKeyAllowed(cmd.Key) {
				log.Printf("Key %s is not allowed", cmd.Key)
				client.writeJSON(map[string]interface{}{
					"type":    "error",
					"message": "Key " + cmd.Key + " is not allowed",
				})
				return
			}
			// Validate key contains no AppleScript injection characters
			if strings.ContainsAny(cmd.Key, "\"\\") {
				log.Printf("Invalid key rejected: %q", cmd.Key)
				client.writeJSON(map[string]interface{}{
					"type":    "error",
					"message": "Invalid key value",
				})
				return
			}
			// Check if all modifiers are allowed
			for _, modifier := range cmd.Modifiers {
				if !isKeyAllowed(modifier) {
					log.Printf("Modifier %s is not allowed", modifier)
					client.writeJSON(map[string]interface{}{
						"type":    "error",
						"message": "Modifier " + modifier + " is not allowed",
					})
					return
				}
			}
			auditLog(client.IP, "osxKeyPressProcess", fmt.Sprintf("process=%s key=%s modifiers=%v", cmd.ProcessName, cmd.Key, cmd.Modifiers))
			processName, key, mods := cmd.ProcessName, cmd.Key, append([]string(nil), cmd.Modifiers...)
			go func() {
				runOsascript(fmt.Sprintf("tell application %q to activate", processName))
				if script, ok := darwinKeyTapScript(key, mods); ok {
					runOsascript(script)
				} else {
					log.Printf("Unsupported macOS key for osxKeyPressProcess: %q", key)
				}
				log.Printf("Sent OSXKeyPressProcess command to %s", processName)
			}()
		} else {
			log.Println("OSXKeyPressProcess command only supported on macOS")
		}

	case "osxAppleScript":
		// NOTE: This intentionally executes arbitrary AppleScript (like shellRun for shell commands).
		// Disable this command in AllowedCommands if unrestricted AppleScript execution is not desired.
		if runtime.GOOS == "darwin" {
			auditLog(client.IP, "osxAppleScript", truncateForLog(cmd.Msg, 100))
			msg := cmd.Msg
			go func() {
				// osxAppleScript intentionally runs arbitrary operator-supplied AppleScript;
				// it requires authentication and can be disabled via AllowedCommands.
				// nosec
				if err := exec.Command("osascript", "-e", msg).Run(); err != nil {
					log.Printf("OSXAppleScript error: %v", err)
				}
				log.Printf("Executed OSXAppleScript: %s", truncateForLog(msg, 100))
			}()
		} else {
			log.Println("OSXAppleScript command only supported on macOS")
		}

	case "keyString":
		if cmd.Msg == "" {
			client.writeJSON(map[string]interface{}{
				"type":    "error",
				"message": "Empty string",
			})
			return
		}
		// Validate each character is allowed in restricted mode
		for _, r := range cmd.Msg {
			if !isKeyAllowed(string(r)) {
				log.Printf("Character %q in keyString is not allowed", string(r))
				auditLog(client.IP, "KEY_BLOCKED", fmt.Sprintf("keyString contains blocked char=%q", string(r)))
				_ = client.writeJSON(map[string]interface{}{
					"type":    "error",
					"message": "Character " + string(r) + " is not allowed",
				})
				return
			}
		}
		auditLog(client.IP, "keyString", truncateForLog(cmd.Msg, 100))
		typeString(cmd.Msg)
		// Typed text can contain credentials; keep stdout as terse as the audit log.
		log.Printf("Typed string: %s", truncateForLog(cmd.Msg, 100))

	case "shellRun":
		// shellRun intentionally executes arbitrary operator-supplied shell commands;
		// it requires authentication and can be disabled via AllowedCommands. The
		// command injection it "enables" is the documented purpose of this feature.
		auditLog(client.IP, "shellRun", truncateForLog(cmd.Shell, 100))
		shell := cmd.Shell
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), shellTimeout)
			defer cancel()
			var command *exec.Cmd
			if runtime.GOOS == "windows" {
				// nosec
				command = exec.CommandContext(ctx, "cmd", "/C", shell)
			} else {
				// nosec
				command = exec.CommandContext(ctx, "sh", "-c", shell)
			}
			hideCmdWindow(command)
			out, err := command.CombinedOutput()
			if err != nil {
				log.Printf("Shell command error: %v", err)
			}
			log.Printf("Shell output: %s", truncateForLog(string(out), 500))
		}()

	case "fileOpen":
		target, err := sanitizeOpenTarget(cmd.Path)
		if err != nil {
			log.Printf("fileOpen rejected: %v", err)
			auditLog(client.IP, "BLOCKED", fmt.Sprintf("fileOpen invalid path: %v", err))
			client.writeJSON(map[string]interface{}{
				"type":    "error",
				"message": "Invalid path: " + err.Error(),
			})
			return
		}
		auditLog(client.IP, "fileOpen", fmt.Sprintf("path=%s", target))
		var command *exec.Cmd
		if runtime.GOOS == "windows" {
			// Open via rundll32's FileProtocolHandler rather than "cmd /C start" so
			// the validated target is never parsed by a shell (no command injection).
			// nosec
			command = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
		} else if runtime.GOOS == "darwin" {
			// target is validated (no leading dash / control chars) and passed as a
			// single argument with no shell, so it cannot inject flags or commands.
			// nosec
			command = exec.Command("open", target)
		} else {
			// nosec
			command = exec.Command("xdg-open", target)
		}
		hideCmdWindow(command)
		if err := command.Start(); err != nil {
			log.Printf("File open error: %v", err)
		} else {
			go func() { _ = command.Wait() }()
		}
		log.Printf("Opened file: %s", target)

	case "mousePositionSet":
		x, err1 := strconv.Atoi(cmd.X)
		y, err2 := strconv.Atoi(cmd.Y)
		if err1 == nil && err2 == nil {
			auditLog(client.IP, "mousePositionSet", fmt.Sprintf("x=%d y=%d", x, y))
			moveMouse(x, y)
			log.Printf("Moved mouse to: (%d, %d)", x, y)
		} else {
			log.Println("Invalid mouse coordinates")
		}

	case "mousePositionGet":
		x, y := getMousePos()
		auditLog(client.IP, "mousePositionGet", fmt.Sprintf("x=%d y=%d", x, y))
		response := map[string]interface{}{
			"type": "mousePositionGetResponse",
			"x":    x,
			"y":    y,
		}
		_ = client.writeJSON(response)
		log.Printf("Sent mouse position: (%d, %d)", x, y)

	case "mouseClick":
		double := false
		if cmd.Double == "true" {
			double = true
		}
		auditLog(client.IP, "mouseClick", fmt.Sprintf("button=%s double=%v", cmd.Button, double))
		clickMouse(cmd.Button, double)
		log.Printf("Mouse click: button %s, double: %v", cmd.Button, double)

	default:
		log.Printf("Unknown command type: %s", cmd.Type)
	}
}

// sendSysInfo pushes one metrics update. Returns false only when the WebSocket
// write fails (caller should end the subscription). Metric collection errors
// are logged and the subscription continues.
func sendSysInfo(client *Client) bool {
	sysInfo, err := getSystemInfo()
	if err != nil {
		log.Printf("Error getting system info: %v", err)
		return true
	}
	if err := client.writeJSON(map[string]interface{}{
		"type":      "sysInfoUpdate",
		"cpu":       sysInfo.CPU,
		"maxCPU":    sysInfo.MaxCPU,
		"mem":       sysInfo.Memory,
		"maxMem":    sysInfo.MaxMemory,
		"processes": sysInfo.ProcessCount,
	}); err != nil {
		log.Printf("Error sending sysInfo update: %v", err)
		return false
	}
	return true
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("WebSocket upgrade error:", err)
		return
	}
	// Create the client with a new salt.
	salt := generateSalt()
	remoteIP := r.RemoteAddr
	// Strip port from IP if present (e.g., "192.168.1.5:54321" -> "192.168.1.5")
	if host, _, err := net.SplitHostPort(remoteIP); err == nil {
		remoteIP = host
	}
	client := &Client{
		Conn:          conn,
		Salt:          salt,
		IP:            remoteIP,
		Subscriptions: make(map[string]chan bool),
	}
	registerClient(client)
	defer unregisterClient(client)

	auditLog(remoteIP, "CONNECTED", "")

	// Send auth challenge.
	_ = client.writeJSON(map[string]interface{}{
		"type": "authChallenge",
		"salt": salt,
	})

	// Read loop.
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			log.Println("Read error:", err)
			break
		}
		var cmd Command
		if err := json.Unmarshal(message, &cmd); err != nil {
			log.Println("JSON unmarshal error:", err)
			continue
		}
		normalizeLegacyCommand(&cmd)
		handleCommand(client, cmd)
	}

	// Cleanup: stop any active subscriptions.
	client.subMu.Lock()
	names := make([]string, 0, len(client.Subscriptions))
	for key := range client.Subscriptions {
		names = append(names, key)
	}
	client.subMu.Unlock()
	for _, key := range names {
		stopSubscription(client, key)
		log.Printf("Cleaned up subscription: %s", key)
	}
	conn.Close()
	log.Println("Cleaned up client connection and subscriptions")

	auditLog(client.IP, "DISCONNECTED", "")

	if client.authenticated.Load() {
		decrementConnections()
	}
}

// -------------------------
// Connection Status Management
// -------------------------

func authenticatedClientIPs() []string {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	ips := make([]string, 0, len(clients))
	seen := make(map[string]struct{})
	for c := range clients {
		if !c.authenticated.Load() || c.IP == "" {
			continue
		}
		if _, ok := seen[c.IP]; ok {
			continue
		}
		seen[c.IP] = struct{}{}
		ips = append(ips, c.IP)
	}
	sort.Strings(ips)
	return ips
}

// statusCardAlpha is the soft fill opacity for tinted status cards.
const statusCardAlpha uint8 = 28

// tintStatusColor returns base with a fixed alpha for soft status card backgrounds.
func tintStatusColor(base color.Color, alpha uint8) color.Color {
	r, g, b, _ := base.RGBA()
	return color.NRGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: alpha}
}

func setStatusCardBgColor(bg *canvas.Rectangle, base color.Color) {
	if bg == nil {
		return
	}
	bg.FillColor = tintStatusColor(base, statusCardAlpha)
	canvas.Refresh(bg)
}

func setStatusCardBg(bg *canvas.Rectangle, ok bool) {
	base := theme.ErrorColor()
	if ok {
		base = theme.SuccessColor()
	}
	setStatusCardBgColor(bg, base)
}

// newStatusCard builds a labeled status tile with a tintable background.
// Caption and detail use foreground; primary carries the status color.
func newStatusCard(caption string, primary, detail *canvas.Text, bg *canvas.Rectangle) fyne.CanvasObject {
	captionText := canvas.NewText(strings.ToUpper(caption), theme.ForegroundColor())
	captionText.TextStyle = fyne.TextStyle{Bold: true}
	captionText.TextSize = 11
	bg.CornerRadius = 8
	inner := container.NewPadded(container.NewVBox(captionText, primary, detail))
	return container.NewStack(bg, inner)
}

// refreshConnectionStatus updates the status text based on activeConnections count.
// Must be called with connMu held.
func refreshConnectionStatus() {
	var status, detail string
	var col color.Color
	ok := activeConnections > 0
	if ok {
		status = fmt.Sprintf("Connected (%d client", activeConnections)
		if activeConnections > 1 {
			status += "s"
		}
		status += ")"
		col = theme.SuccessColor()
		ips := authenticatedClientIPs()
		switch {
		case len(ips) == 0:
			detail = "Waiting for authentication…"
		case len(ips) > 3:
			detail = strings.Join(ips[:3], ", ") + "…"
		default:
			detail = strings.Join(ips, ", ")
		}
	} else {
		status = "No connections"
		detail = "Waiting for WebSocket clients"
		col = theme.ErrorColor()
	}
	connectionStatus = status
	if connectionStatusText != nil {
		connectionStatusText.Text = status
		connectionStatusText.Color = col
		canvas.Refresh(connectionStatusText)
	}
	if connectionDetailText != nil {
		connectionDetailText.Text = detail
		canvas.Refresh(connectionDetailText)
	}
	setStatusCardBg(connectionStatusBg, ok)
	// Tray menu rebuild takes connMu; run outside this lock.
	if refreshTrayMenu != nil {
		go refreshTrayMenu()
	}
}

func setBindResult(addr string, err error) {
	bindStatusMu.Lock()
	lastBindAddr = addr
	lastBindError = err
	bindStatusMu.Unlock()
	refreshListenStatusUI()
}

func refreshListenStatusUI() {
	if listenStatusText == nil {
		return
	}
	bindStatusMu.Lock()
	err := lastBindError
	addr := lastBindAddr
	bindStatusMu.Unlock()
	if addr == "" {
		configMu.RLock()
		addr = config.Interface + ":" + config.Port
		configMu.RUnlock()
	}
	ok := err == nil
	if !ok {
		listenStatusText.Text = "Bind failed"
		listenStatusText.Color = theme.ErrorColor()
		if listenDetailText != nil {
			listenDetailText.Text = err.Error()
			canvas.Refresh(listenDetailText)
		}
	} else {
		listenStatusText.Text = "Listening"
		listenStatusText.Color = theme.SuccessColor()
		if listenDetailText != nil {
			listenDetailText.Text = addr
			canvas.Refresh(listenDetailText)
		}
	}
	canvas.Refresh(listenStatusText)
	setStatusCardBg(listenStatusBg, ok)
}

func preferredLocalIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1"
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			if v4 := ip.To4(); v4 != nil {
				return v4.String()
			}
		}
	}
	return "127.0.0.1"
}

// websocketTargetHost is what WebSocket clients should use as their "Target IP" field.
func websocketTargetHost() string {
	configMu.RLock()
	iface := config.Interface
	configMu.RUnlock()
	if iface == "" || iface == "0.0.0.0" {
		return "127.0.0.1"
	}
	return iface
}

func flashButtonText(btn *widget.Button, temp, original string) {
	btn.SetText(temp)
	time.AfterFunc(1500*time.Millisecond, func() {
		btn.SetText(original)
	})
}

func flashLabelText(lbl *widget.Label, temp, original string) {
	if lbl == nil {
		return
	}
	lbl.SetText(temp)
	time.AfterFunc(1500*time.Millisecond, func() {
		lbl.SetText(original)
	})
}

func openAccessibilitySettings() {
	if runtime.GOOS != "darwin" {
		return
	}
	urls := []string{
		"x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility",
		"x-apple.systempreferences:com.apple.settings.PrivacySecurity.extension",
	}
	for _, u := range urls {
		if err := exec.Command("open", u).Start(); err == nil {
			return
		}
	}
	_ = exec.Command("open", "/System/Library/PreferencePanes/Security.prefPane").Start()
}

func incrementConnections() {
	connMu.Lock()
	defer connMu.Unlock()
	activeConnections++
	refreshConnectionStatus()
}

func decrementConnections() {
	connMu.Lock()
	defer connMu.Unlock()
	if activeConnections > 0 {
		activeConnections--
	}
	refreshConnectionStatus()
}

// -------------------------
// HTTP Server Management
// -------------------------

var (
	serverMu       sync.Mutex
	currentServer  *http.Server
	bindGeneration uint64 // bumps on each bind attempt; stale attempts must not win
)

// startHTTPServer binds synchronously (used at process start before the GUI runs).
func startHTTPServer(bindIP, port string, errorCallback func(err error)) {
	applied, err := bindHTTPServer(bindIP, port)
	if applied && errorCallback != nil {
		errorCallback(err)
	}
}

// restartServer stops the current listener (kicking clients) and rebinds off the
// caller's goroutine so the Fyne UI thread never blocks on Shutdown.
func restartServer(bindIP, port string, errorCallback func(err error)) {
	go func() {
		log.Printf("Restarting server to bind on %s:%s", bindIP, port)
		applied, err := bindHTTPServer(bindIP, port)
		if !applied || errorCallback == nil {
			return
		}
		errorCallback(err)
	}()
}

// bindHTTPServer is the serialized stop+start path. A newer bind bumps
// bindGeneration so an in-flight older attempt cannot orphan a listener or
// overwrite currentServer / bind UI status.
//
// Returns applied=false when this attempt was superseded by a newer one.
func bindHTTPServer(bindIP, port string) (applied bool, err error) {
	serverMu.Lock()
	bindGeneration++
	gen := bindGeneration
	old := currentServer
	currentServer = nil
	serverMu.Unlock()

	if old != nil {
		// Drop existing WS sessions so Shutdown can finish and no orphan
		// handlers keep accepting remote commands against the old bind.
		disconnectAllClients()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if shutErr := old.Shutdown(ctx); shutErr != nil {
			log.Printf("Error shutting down server: %v", shutErr)
		} else {
			log.Println("Server shut down successfully")
		}
		cancel()
	}

	addr := bindIP + ":" + port
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsHandler)
	ln, listenErr := net.Listen("tcp", addr)

	serverMu.Lock()
	defer serverMu.Unlock()
	if gen != bindGeneration {
		if ln != nil {
			_ = ln.Close()
		}
		return false, nil
	}
	if listenErr != nil {
		setBindResult(addr, listenErr)
		return true, listenErr
	}

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
		// ReadHeaderTimeout bounds how long a client may take to send request
		// headers, mitigating Slowloris-style DoS. ReadTimeout/WriteTimeout are
		// intentionally left unset because /ws upgrades to long-lived WebSocket
		// connections that must not be force-closed mid-session.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	currentServer = srv
	setBindResult(addr, nil)
	go func() {
		log.Printf("Server starting on %s", addr)
		if serveErr := srv.Serve(ln); serveErr != nil && serveErr != http.ErrServerClosed {
			log.Printf("Server error: %v", serveErr)
			serverMu.Lock()
			stillCurrent := currentServer == srv
			serverMu.Unlock()
			if stillCurrent {
				setBindResult(addr, serveErr)
			}
		}
	}()
	return true, nil
}

// -------------------------
// Network Interface Options
// -------------------------

func getInterfaceOptions() ([]string, map[string]string) {
	options := []string{"0.0.0.0"}
	mapping := map[string]string{"0.0.0.0": "0.0.0.0"}
	ifaces, err := net.Interfaces()
	if err != nil {
		log.Println("Error getting interfaces:", err)
		return options, mapping
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			ip = ip.To4()
			if ip == nil {
				continue
			}
			display := iface.Name + " - " + ip.String()
			options = append(options, display)
			mapping[display] = ip.String()
			break
		}
	}
	return options, mapping
}

// -------------------------
// GUI using Fyne
// -------------------------

// Map of command names to human-readable descriptions
var commandDescriptions = map[string]string{
	"keyPress":            "Single Key Press",
	"keyCombinationPress": "Key Combination",
	"keyDown":             "Key Down (Hold)",
	"keyUp":               "Key Up (Release)",
	"osxKeyPressProcess":  "macOS Process Key Press",
	"osxAppleScript":      "macOS AppleScript",
	"keyString":           "Type Text String",
	"shellRun":            "Run Shell Command",
	"fileOpen":            "Open File",
	"mousePositionSet":    "Set Mouse Position",
	"mousePositionGet":    "Get Mouse Position",
	"mouseClick":          "Mouse Click",
	"subscribe":           "Event Subscriptions",
}

func showAllowedCommandsDialog(parent fyne.Window, updateStatusCallback func()) {
	// Snapshot the current state; config is mutated from the WebSocket goroutines.
	configMu.RLock()
	allowedNow := make(map[string]bool, len(config.AllowedCommands))
	for cmd, ok := range config.AllowedCommands {
		allowedNow[cmd] = ok
	}
	configMu.RUnlock()

	// Create checkboxes for each command.
	checkboxes := make(map[string]*widget.Check)
	var checkboxList []fyne.CanvasObject
	for _, cmd := range defaultCommands {
		// Use human-readable description if available, otherwise use the command name
		description, exists := commandDescriptions[cmd]
		if !exists {
			description = cmd
		}
		cb := widget.NewCheck(description, nil)
		cb.SetChecked(allowedNow[cmd])
		checkboxes[cmd] = cb
		checkboxList = append(checkboxList, cb)
	}
	// Arrange checkboxes in a grid with 3 columns.
	grid := container.NewGridWithColumns(3, checkboxList...)

	// Declare the dialog variable.
	var customDialog dialog.Dialog

	// Create our own Save and Cancel buttons.
	saveButton := widget.NewButton("Save", func() {
		configMu.Lock()
		var changed []string
		for cmd, cb := range checkboxes {
			if config.AllowedCommands[cmd] != cb.Checked {
				action := "disabled"
				if cb.Checked {
					action = "enabled"
				}
				changed = append(changed, fmt.Sprintf("%s=%s", cmd, action))
			}
			config.AllowedCommands[cmd] = cb.Checked
		}
		err := saveConfigLocked(&config)
		configMu.Unlock()
		if len(changed) > 0 {
			auditLog("gui", "COMMANDS_CHANGED", strings.Join(changed, ", "))
		}
		if err != nil {
			dialog.ShowError(err, parent)
		}
		if updateStatusCallback != nil {
			updateStatusCallback()
		}
		customDialog.Hide()
	})
	cancelButton := widget.NewButton("Cancel", func() {
		customDialog.Hide()
	})
	// Arrange the buttons in a grid with 2 columns.
	buttons := container.NewGridWithColumns(2, saveButton, cancelButton)

	// Create overall container with the grid of checkboxes and our buttons.
	content := container.NewVBox(grid, buttons)

	// Create a custom dialog with no extra dismiss button.
	customDialog = dialog.NewCustomWithoutButtons("Allowed Remote Actions", content, parent)
	customDialog.Show()
}

// -------------------------
// Key Control Configuration
// -------------------------

// Key categories for easier selection
var keyCategories = map[string][]string{
	"Navigation": {"up", "down", "left", "right", "home", "end", "pageup", "pagedown", "tab", "backspace", "delete", "insert"},
	"Function":   {"f1", "f2", "f3", "f4", "f5", "f6", "f7", "f8", "f9", "f10", "f11", "f12"},
	"Media":      {"play", "pause", "stop", "next", "previous", "volumeup", "volumedown", "mute"},
	"System":     {"esc", "enter", "space", "print", "scroll", "pause", "break"},
	"Modifiers":  {"ctrl", "alt", "shift", "command", "win"},
	"Numbers":    {"0", "1", "2", "3", "4", "5", "6", "7", "8", "9"},
	"Letters":    {"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m", "n", "o", "p", "q", "r", "s", "t", "u", "v", "w", "x", "y", "z"},
	"Symbols":    {"!", "@", "#", "$", "%", "^", "&", "*", "(", ")", "-", "_", "=", "+", "[", "]", "{", "}", "\\", "|", ";", ":", "'", "\"", ",", ".", "/", "?", "<", ">", "`", "~"},
}

// Human-readable names for special keys
var keyDisplayNames = map[string]string{
	"up":         "↑ Up Arrow",
	"down":       "↓ Down Arrow",
	"left":       "← Left Arrow",
	"right":      "→ Right Arrow",
	"home":       "Home",
	"end":        "End",
	"pageup":     "Page Up",
	"pagedown":   "Page Down",
	"tab":        "Tab",
	"backspace":  "Backspace",
	"delete":     "Delete",
	"insert":     "Insert",
	"esc":        "Escape",
	"enter":      "Enter",
	"space":      "Space",
	"print":      "Print Screen",
	"scroll":     "Scroll Lock",
	"pause":      "Pause",
	"break":      "Break",
	"play":       "Play",
	"stop":       "Stop",
	"next":       "Next Track",
	"previous":   "Previous Track",
	"volumeup":   "Volume Up",
	"volumedown": "Volume Down",
	"mute":       "Mute",
	"ctrl":       "Ctrl",
	"alt":        "Alt",
	"shift":      "Shift",
	"command":    "Cmd (Mac)",
	"win":        "Windows",
}

// Get display name for a key
func getKeyDisplayName(key string) string {
	if displayName, exists := keyDisplayNames[key]; exists {
		return displayName
	}
	// For single characters, capitalize them
	if len(key) == 1 {
		return strings.ToUpper(key)
	}
	return key
}

// normalizeKeyAlias maps common client-side key aliases to the canonical names
// used in keyCategories so that isKeyAllowed lookups match correctly.
func normalizeKeyAlias(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	switch key {
	case " ", "spacebar":
		return "space"
	case "cmd", "meta", "super":
		return "command"
	case "control":
		return "ctrl"
	case "option":
		return "alt"
	case "return":
		return "enter"
	case "escape":
		return "esc"
	default:
		return key
	}
}

// Check if a key is allowed based on current configuration
func isKeyAllowed(key string) bool {
	key = normalizeKeyAlias(key)

	configMu.RLock()
	defer configMu.RUnlock()

	// If full access mode is enabled, allow all keys
	if config.KeyControlMode == "full_access" {
		return true
	}

	// Check if the specific key is allowed
	if allowed, exists := config.AllowedKeys[key]; exists && allowed {
		return true
	}

	// Check if the key's category is allowed
	for category, keys := range keyCategories {
		if categoryAllowed, exists := config.KeyCategories[category]; exists && categoryAllowed {
			for _, categoryKey := range keys {
				if categoryKey == key {
					return true
				}
			}
		}
	}

	return false
}

func showKeyControlDialog(parent fyne.Window, updateStatusCallback func()) {
	// Snapshot the current state; config is mutated from the WebSocket goroutines.
	configMu.RLock()
	selectedMode := config.KeyControlMode
	categoriesNow := make(map[string]bool, len(config.KeyCategories))
	for cat, ok := range config.KeyCategories {
		categoriesNow[cat] = ok
	}
	keysNow := make(map[string]bool, len(config.AllowedKeys))
	for key, ok := range config.AllowedKeys {
		keysNow[key] = ok
	}
	configMu.RUnlock()

	categoryCheckboxes := make(map[string]*widget.Check)
	individualKeyCheckboxes := make(map[string]*widget.Check)

	countLabel := widget.NewLabel("")
	countLabel.Importance = widget.MediumImportance

	var updateSelectionCount func()
	updateSelectionCount = func() {
		if selectedMode == "full_access" {
			countLabel.SetText("Selection: all keys allowed")
			return
		}
		nCats, nKeys := 0, 0
		for _, cb := range categoryCheckboxes {
			if cb.Checked {
				nCats++
			}
		}
		for _, cb := range individualKeyCheckboxes {
			if cb.Checked {
				nKeys++
			}
		}
		countLabel.SetText(fmt.Sprintf("Selection: %d categories, %d individual keys", nCats, nKeys))
	}

	fullAccessRadio := widget.NewRadioGroup([]string{"Full Access (All Keys)", "Restricted (Selected Keys Only)"}, func(selected string) {
		if selected == "Full Access (All Keys)" {
			selectedMode = "full_access"
		} else {
			selectedMode = "restricted"
		}
		updateSelectionCount()
	})
	if selectedMode == "full_access" {
		fullAccessRadio.SetSelected("Full Access (All Keys)")
	} else {
		fullAccessRadio.SetSelected("Restricted (Selected Keys Only)")
	}

	sortedCategories := make([]string, 0, len(keyCategories))
	for category := range keyCategories {
		sortedCategories = append(sortedCategories, category)
	}
	sort.Strings(sortedCategories)

	acc := widget.NewAccordion()
	for _, category := range sortedCategories {
		cat := category
		keys := keyCategories[cat]
		cb := widget.NewCheck(fmt.Sprintf("Allow entire “%s” category (%d keys)", cat, len(keys)), func(bool) {
			updateSelectionCount()
		})
		cb.SetChecked(categoriesNow[cat])
		categoryCheckboxes[cat] = cb

		var keyObjs []fyne.CanvasObject
		for _, k := range keys {
			lbl := widget.NewLabel(getKeyDisplayName(k))
			keyObjs = append(keyObjs, lbl)
		}
		acc.Append(widget.NewAccordionItem(
			fmt.Sprintf("%s (%d)", cat, len(keys)),
			container.NewVBox(cb, container.NewGridWithColumns(4, keyObjs...)),
		))
	}

	commonKeys := []string{
		"space", "enter", "esc", "tab", "backspace", "delete",
		"up", "down", "left", "right", "home", "end",
	}
	for _, key := range commonKeys {
		k := key
		cb := widget.NewCheck(getKeyDisplayName(k), func(bool) { updateSelectionCount() })
		cb.SetChecked(keysNow[k])
		individualKeyCheckboxes[k] = cb
	}
	updateSelectionCount()

	keyGridHolder := container.NewStack()
	rebuildKeyGrid := func(filter string) {
		filter = strings.ToLower(strings.TrimSpace(filter))
		var objs []fyne.CanvasObject
		for _, key := range commonKeys {
			name := getKeyDisplayName(key)
			if filter != "" &&
				!strings.Contains(strings.ToLower(name), filter) &&
				!strings.Contains(strings.ToLower(key), filter) {
				continue
			}
			objs = append(objs, individualKeyCheckboxes[key])
		}
		if len(objs) == 0 {
			keyGridHolder.Objects = []fyne.CanvasObject{widget.NewLabel("No keys match filter")}
		} else {
			keyGridHolder.Objects = []fyne.CanvasObject{container.NewGridWithColumns(3, objs...)}
		}
		keyGridHolder.Refresh()
	}
	rebuildKeyGrid("")

	searchEntry := widget.NewEntry()
	searchEntry.SetPlaceHolder("Filter individual keys…")
	searchEntry.OnChanged = rebuildKeyGrid

	content := container.NewVBox(
		widget.NewLabel("Choose how to control which keys can be pressed remotely:"),
		fullAccessRadio,
		countLabel,
		widget.NewSeparator(),
		widget.NewLabel("Key Categories (expand for details):"),
		acc,
		widget.NewSeparator(),
		widget.NewLabel("Individual Keys:"),
		searchEntry,
		keyGridHolder,
	)
	scrollContainer := container.NewVScroll(container.NewPadded(content))

	var customDialog dialog.Dialog
	saveButton := widget.NewButton("Save", func() {
		configMu.Lock()
		var changes []string
		if config.KeyControlMode != selectedMode {
			changes = append(changes, fmt.Sprintf("mode=%s", selectedMode))
		}
		config.KeyControlMode = selectedMode
		for category, cb := range categoryCheckboxes {
			if config.KeyCategories[category] != cb.Checked {
				action := "disabled"
				if cb.Checked {
					action = "enabled"
				}
				changes = append(changes, fmt.Sprintf("category:%s=%s", category, action))
			}
			config.KeyCategories[category] = cb.Checked
		}
		for key, cb := range individualKeyCheckboxes {
			if config.AllowedKeys[key] != cb.Checked {
				action := "disabled"
				if cb.Checked {
					action = "enabled"
				}
				changes = append(changes, fmt.Sprintf("key:%s=%s", getKeyDisplayName(key), action))
			}
			config.AllowedKeys[key] = cb.Checked
		}
		err := saveConfigLocked(&config)
		configMu.Unlock()
		if len(changes) > 0 {
			auditLog("gui", "KEY_ACCESS_CHANGED", strings.Join(changes, ", "))
		}
		if err != nil {
			dialog.ShowError(err, parent)
		}
		if updateStatusCallback != nil {
			updateStatusCallback()
		}
		customDialog.Hide()
	})
	saveButton.Importance = widget.HighImportance
	cancelButton := widget.NewButton("Cancel", func() { customDialog.Hide() })

	bottomBar := container.NewVBox(
		widget.NewSeparator(),
		container.NewHBox(layout.NewSpacer(), saveButton, cancelButton, layout.NewSpacer()),
	)
	customDialog = dialog.NewCustomWithoutButtons(
		"Key Access Control",
		container.NewBorder(nil, bottomBar, nil, nil, scrollContainer),
		parent,
	)
	customDialog.Resize(fyne.NewSize(680, 640))
	customDialog.Show()
}

func showActivityLogDialog(parent fyne.Window) {
	type logRow struct{ stamp, message, full string }
	var lines []logRow
	logList := widget.NewList(
		func() int { return len(lines) },
		func() fyne.CanvasObject {
			stamp := widget.NewRichText(&widget.TextSegment{
				Style: widget.RichTextStyle{
					SizeName:  theme.SizeNameCaptionText,
					TextStyle: fyne.TextStyle{Monospace: true},
					Inline:    true,
				},
			})
			msg := widget.NewLabel("")
			msg.TextStyle = fyne.TextStyle{Monospace: true}
			msg.Truncation = fyne.TextTruncateEllipsis
			return container.NewBorder(nil, nil, stamp, nil, msg)
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			objs := o.(*fyne.Container).Objects
			msg := objs[0].(*widget.Label)
			stamp := objs[1].(*widget.RichText)
			stamp.Segments[0].(*widget.TextSegment).Text = lines[i].stamp
			stamp.Refresh()
			msg.SetText(lines[i].message)
		},
	)
	var copyHint *widget.Label
	// Selecting a row copies the full line. Unselect immediately so the same
	// row can be copied again and no stale highlight survives a refresh.
	logList.OnSelected = func(i widget.ListItemID) {
		if i < len(lines) {
			parent.Clipboard().SetContent(lines[i].full)
			flashLabelText(copyHint, "Copied line to clipboard", "Click a line to copy it.")
		}
		logList.UnselectAll()
	}

	lastSeq := ^uint64(0)
	refreshLog := func() {
		entries, seq := audit.Snapshot()
		if seq == lastSeq {
			return
		}
		lastSeq = seq
		next := make([]logRow, 0, len(entries))
		for _, e := range entries {
			next = append(next, logRow{
				stamp:   e.Time.Format("2006-01-02 15:04:05"),
				message: fmt.Sprintf("[%s] %s %s", e.IP, e.Action, e.Detail),
				full:    e.String(),
			})
		}
		lines = next
		logList.Refresh()
		if len(lines) > 0 {
			logList.ScrollToBottom()
		}
	}
	refreshLog()

	// Auto-refresh every second for near-realtime view
	done := make(chan struct{})
	var doneOnce sync.Once
	stopRefresh := func() { doneOnce.Do(func() { close(done) }) }
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				refreshLog()
			case <-done:
				return
			}
		}
	}()

	var logDialog dialog.Dialog

	closeBtn := widget.NewButton("Close", func() {
		stopRefresh()
		logDialog.Hide()
	})

	logFilePath := ""
	if audit != nil {
		logFilePath = audit.filePath
	}
	fileLabel := widget.NewLabel("Log file: " + logFilePath)
	fileLabel.TextStyle = fyne.TextStyle{Italic: true}

	var copyPathBtn *widget.Button
	copyPathBtn = widget.NewButton("Copy Path", func() {
		parent.Clipboard().SetContent(logFilePath)
		flashButtonText(copyPathBtn, "Copied", "Copy Path")
	})

	copyHint = widget.NewLabel("Click a line to copy it.")
	copyHint.TextStyle = fyne.TextStyle{Italic: true}

	bottomBar := container.NewVBox(
		copyHint,
		container.NewBorder(nil, nil, fileLabel, copyPathBtn),
		container.NewHBox(layout.NewSpacer(), closeBtn, layout.NewSpacer()),
	)

	content := container.NewBorder(nil, container.NewPadded(bottomBar), nil, nil, logList)

	logDialog = dialog.NewCustomWithoutButtons("Activity Log", content, parent)
	logDialog.SetOnClosed(func() {
		stopRefresh()
	})
	logDialog.Resize(fyne.NewSize(750, 500))
	logDialog.Show()
}

func startGUI() {
	myApp := app.New()
	appIcon := fyne.NewStaticResource("icon.png", iconPNG)
	myApp.SetIcon(appIcon)
	w := myApp.NewWindow("Bitfocus Listener")

	// Ignore bind-change handlers until the window is fully wired. Fyne Select
	// calls OnChanged from SetSelected, which previously restarted the server
	// on every launch and dropped WebSocket connections.
	guiReady := false
	var errorState string

	portEntry := widget.NewEntry()
	portEntry.SetText(config.Port)
	portEntry.Validator = func(string) error {
		if errorState != "" {
			return errors.New(errorState)
		}
		return nil
	}

	applyBindUI := func(err error) {
		if err != nil {
			errorState = err.Error()
		} else {
			errorState = ""
		}
		refreshListenStatusUI()
		portEntry.Validate()
	}

	restartWithCurrentBind := func() {
		configMu.RLock()
		iface, port := config.Interface, config.Port
		configMu.RUnlock()
		restartServer(iface, port, applyBindUI)
	}

	connMu.Lock()
	connectionStatusText = canvas.NewText("No connections", theme.ErrorColor())
	connectionStatusText.TextStyle = fyne.TextStyle{Bold: true}
	connectionDetailText = canvas.NewText("Waiting for WebSocket clients", theme.ForegroundColor())
	connectionDetailText.TextSize = 12
	connectionStatusBg = canvas.NewRectangle(tintStatusColor(theme.ErrorColor(), statusCardAlpha))
	listenStatusText = canvas.NewText("Listening", theme.SuccessColor())
	listenStatusText.TextStyle = fyne.TextStyle{Bold: true}
	listenDetailText = canvas.NewText("", theme.ForegroundColor())
	listenDetailText.TextSize = 12
	listenStatusBg = canvas.NewRectangle(tintStatusColor(theme.SuccessColor(), statusCardAlpha))
	refreshConnectionStatus()
	connMu.Unlock()
	refreshListenStatusUI()

	trayHint := canvas.NewText("", theme.PlaceHolderColor())
	trayHint.TextSize = 11
	trayHint.Alignment = fyne.TextAlignCenter

	trayEnabled := false
	var deskApp desktop.App
	if desk, ok := myApp.(desktop.App); ok {
		trayEnabled = true
		deskApp = desk
		desk.SetSystemTrayIcon(trayIconResource(appIcon))
		trayHint.Text = "Runs in the menu bar when this window is closed."
	} else {
		trayHint.Text = "Quit from the window close button when tray is unavailable."
	}

	buildTrayMenu := func() {
		if deskApp == nil {
			return
		}
		connMu.Lock()
		status := connectionStatus
		connMu.Unlock()
		statusItem := fyne.NewMenuItem(status, nil)
		statusItem.Disabled = true
		quitItem := fyne.NewMenuItem("Quit", func() { myApp.Quit() })
		quitItem.IsQuit = true
		deskApp.SetSystemTrayMenu(fyne.NewMenu("Bitfocus Listener",
			statusItem,
			fyne.NewMenuItem("Open Settings", func() {
				w.Show()
				w.RequestFocus()
			}),
			fyne.NewMenuItem("Restart Server", restartWithCurrentBind),
			fyne.NewMenuItem("Copy Target Host", func() {
				w.Clipboard().SetContent(websocketTargetHost())
			}),
			fyne.NewMenuItem("Activity Log", func() {
				w.Show()
				w.RequestFocus()
				showActivityLogDialog(w)
			}),
			fyne.NewMenuItemSeparator(),
			quitItem,
		))
	}
	refreshTrayMenu = buildTrayMenu
	buildTrayMenu()

	w.SetCloseIntercept(func() {
		if trayEnabled {
			w.Hide()
			return
		}
		myApp.Quit()
	})
	myApp.Lifecycle().SetOnStarted(func() { hideDockIcon() })

	options, ifaceMapping := getInterfaceOptions()
	initialIface := "0.0.0.0"
	for disp, ip := range ifaceMapping {
		if ip == config.Interface {
			initialIface = disp
			break
		}
	}
	ifaceSelect := widget.NewSelect(options, func(selected string) {
		if !guiReady {
			return
		}
		newIP := ifaceMapping[selected]
		configMu.Lock()
		if config.Interface == newIP {
			configMu.Unlock()
			return
		}
		config.Interface = newIP
		if err := saveConfigLocked(&config); err != nil {
			configMu.Unlock()
			dialog.ShowError(err, w)
			return
		}
		iface, port := config.Interface, config.Port
		configMu.Unlock()
		auditLog("gui", "INTERFACE_CHANGED", fmt.Sprintf("bind=%s", newIP))
		restartServer(iface, port, applyBindUI)
	})
	ifaceSelect.SetSelected(initialIface)

	var portDebounceTimer *time.Timer
	portEntry.OnChanged = func(newPort string) {
		if !guiReady {
			return
		}
		if _, err := strconv.Atoi(newPort); err != nil {
			return
		}
		if portDebounceTimer != nil {
			portDebounceTimer.Stop()
		}
		portDebounceTimer = time.AfterFunc(500*time.Millisecond, func() {
			configMu.Lock()
			if config.Port == newPort {
				configMu.Unlock()
				return
			}
			config.Port = newPort
			if err := saveConfigLocked(&config); err != nil {
				configMu.Unlock()
				log.Printf("Error saving config: %v", err)
				return
			}
			iface, port := config.Interface, config.Port
			configMu.Unlock()
			auditLog("gui", "PORT_CHANGED", fmt.Sprintf("port=%s", newPort))
			restartServer(iface, port, applyBindUI)
		})
	}

	actionsStatusText := canvas.NewText("All enabled", theme.SuccessColor())
	actionsStatusText.TextStyle = fyne.TextStyle{Bold: true}
	actionsDetailText := canvas.NewText("", theme.ForegroundColor())
	actionsDetailText.TextSize = 12
	actionsStatusBg := canvas.NewRectangle(tintStatusColor(theme.SuccessColor(), statusCardAlpha))
	actionsCard := newStatusCard("Remote actions", actionsStatusText, actionsDetailText, actionsStatusBg)
	updateRemoteActionsStatus := func() {
		configMu.RLock()
		enabled, total := 0, len(defaultCommands)
		for _, cmd := range defaultCommands {
			if config.AllowedCommands[cmd] {
				enabled++
			}
		}
		configMu.RUnlock()
		switch {
		case enabled == 0:
			actionsStatusText.Text = "None allowed"
			actionsStatusText.Color = theme.ErrorColor()
			actionsDetailText.Text = fmt.Sprintf("0 of %d actions", total)
			setStatusCardBg(actionsStatusBg, false)
		case enabled == total:
			actionsStatusText.Text = "All enabled"
			actionsStatusText.Color = theme.SuccessColor()
			actionsDetailText.Text = fmt.Sprintf("%d of %d actions", enabled, total)
			setStatusCardBg(actionsStatusBg, true)
		default:
			actionsStatusText.Text = "Restricted"
			actionsStatusText.Color = theme.WarningColor()
			actionsDetailText.Text = fmt.Sprintf("%d of %d actions", enabled, total)
			setStatusCardBgColor(actionsStatusBg, theme.WarningColor())
		}
		canvas.Refresh(actionsStatusText)
		canvas.Refresh(actionsDetailText)
	}
	updateRemoteActionsStatus()

	keyAccessStatusText := canvas.NewText("Full access", theme.SuccessColor())
	keyAccessStatusText.TextStyle = fyne.TextStyle{Bold: true}
	keyAccessDetailText := canvas.NewText("All keys allowed", theme.ForegroundColor())
	keyAccessDetailText.TextSize = 12
	keyAccessStatusBg := canvas.NewRectangle(tintStatusColor(theme.SuccessColor(), statusCardAlpha))
	keyAccessCard := newStatusCard("Key access", keyAccessStatusText, keyAccessDetailText, keyAccessStatusBg)

	keyAccessWarnTitle := widget.NewLabel("No keys allowed")
	keyAccessWarnTitle.TextStyle = fyne.TextStyle{Bold: true}
	keyAccessWarnBody := widget.NewLabel("Remote key presses will be blocked until you enable key categories or individual keys under Security → Key Access Control.")
	keyAccessWarnBody.Wrapping = fyne.TextWrapWord
	keyAccessWarnBg := canvas.NewRectangle(tintStatusColor(theme.ErrorColor(), statusCardAlpha))
	keyAccessWarnBg.CornerRadius = 8
	keyAccessWarning := container.NewStack(
		keyAccessWarnBg,
		container.NewPadded(container.NewVBox(keyAccessWarnTitle, keyAccessWarnBody)),
	)
	updateKeyControlStatus := func() {
		configMu.RLock()
		mode := config.KeyControlMode
		nCats, nKeys := 0, 0
		for _, ok := range config.KeyCategories {
			if ok {
				nCats++
			}
		}
		for _, ok := range config.AllowedKeys {
			if ok {
				nKeys++
			}
		}
		configMu.RUnlock()
		if mode == "full_access" {
			keyAccessStatusText.Text = "Full access"
			keyAccessStatusText.Color = theme.SuccessColor()
			keyAccessDetailText.Text = "All keys allowed"
			setStatusCardBg(keyAccessStatusBg, true)
			keyAccessWarning.Hide()
		} else if nCats == 0 && nKeys == 0 {
			keyAccessStatusText.Text = "Restricted"
			keyAccessStatusText.Color = theme.ErrorColor()
			keyAccessDetailText.Text = "No keys allowed"
			setStatusCardBg(keyAccessStatusBg, false)
			keyAccessWarning.Show()
		} else {
			keyAccessStatusText.Text = "Restricted"
			keyAccessStatusText.Color = theme.WarningColor()
			keyAccessDetailText.Text = fmt.Sprintf("%d categories, %d keys", nCats, nKeys)
			setStatusCardBgColor(keyAccessStatusBg, theme.WarningColor())
			keyAccessWarning.Hide()
		}
		canvas.Refresh(keyAccessStatusText)
		canvas.Refresh(keyAccessDetailText)
	}
	updateKeyControlStatus()

	passwordMasked := true
	entryHeight := portEntry.MinSize().Height
	passwordDisplay := canvas.NewText("", theme.ForegroundColor())
	passwordDisplay.TextSize = theme.TextSize()
	refreshPasswordDisplay := func() {
		pwd := currentPassword()
		if passwordMasked {
			passwordDisplay.Text = strings.Repeat("•", len(pwd))
		} else {
			passwordDisplay.Text = pwd
		}
		canvas.Refresh(passwordDisplay)
	}
	refreshPasswordDisplay()

	// Match Entry styling/height without focus/cursor — password is generate-only.
	passwordBg := canvas.NewRectangle(theme.InputBackgroundColor())
	passwordBg.CornerRadius = theme.InputRadiusSize()
	passwordBg.StrokeColor = theme.InputBorderColor()
	passwordBg.StrokeWidth = theme.InputBorderSize()
	passwordBg.SetMinSize(fyne.NewSize(40, entryHeight))
	innerPad := theme.InnerPadding()
	passwordField := container.NewStack(
		passwordBg,
		container.New(layout.NewCustomPaddedLayout(innerPad, innerPad, innerPad, innerPad), passwordDisplay),
	)

	showHideBtn := widget.NewButton("Show", func() {})
	showHideBtn.OnTapped = func() {
		passwordMasked = !passwordMasked
		if passwordMasked {
			showHideBtn.SetText("Show")
		} else {
			showHideBtn.SetText("Hide")
		}
		refreshPasswordDisplay()
	}

	generateButton := widget.NewButton("Generate New", func() {
		msg := widget.NewLabel("This will disconnect all current clients.\nAre you sure?")
		msg.Wrapping = fyne.TextWrapWord
		var confirmDlg dialog.Dialog
		noBtn := widget.NewButton("No", func() { confirmDlg.Hide() })
		yesBtn := widget.NewButton("Yes", func() {
			confirmDlg.Hide()
			newPass := generatePassword()
			configMu.Lock()
			storedPassword = newPass
			config.Password = newPass
			err := saveConfigLocked(&config)
			configMu.Unlock()
			disconnectAllClients()
			auditLog("gui", "PASSWORD_CHANGED", "new password generated; clients disconnected")
			refreshPasswordDisplay()
			if err != nil {
				dialog.ShowError(err, w)
			}
		})
		yesBtn.Importance = widget.DangerImportance
		confirmDlg = dialog.NewCustomWithoutButtons(
			"Generate New Password",
			container.NewVBox(msg, container.NewGridWithColumns(2, noBtn, yesBtn)),
			w,
		)
		confirmDlg.Show()
	})
	generateButton.Importance = widget.DangerImportance

	var copyButton *widget.Button
	copyButton = widget.NewButton("Copy", func() {
		w.Clipboard().SetContent(currentPassword())
		flashButtonText(copyButton, "Copied", "Copy")
	})

	passwordRow := container.NewBorder(nil, nil, nil,
		container.NewHBox(showHideBtn, copyButton, generateButton),
		// Keep field at Entry height; don't stretch to match taller action buttons.
		container.NewVBox(layout.NewSpacer(), passwordField, layout.NewSpacer()),
	)

	var copyHostBtn *widget.Button
	copyHostBtn = widget.NewButton("Copy Target Host", func() {
		w.Clipboard().SetContent(websocketTargetHost())
		flashButtonText(copyHostBtn, "Copied", "Copy Target Host")
	})

	activityLogBtn := widget.NewButton("Activity Log", func() { showActivityLogDialog(w) })
	activityLogBtn.Importance = widget.LowImportance

	connectionTab := container.NewPadded(container.NewVBox(
		keyAccessWarning,
		container.New(layout.NewFormLayout(),
			widget.NewLabel("Interface"), ifaceSelect,
			widget.NewLabel("Port"), portEntry,
			widget.NewLabel("Password"), passwordRow,
		),
		container.NewGridWithColumns(2,
			copyHostBtn,
			widget.NewButton("Restart Server", restartWithCurrentBind),
		),
		activityLogBtn,
	))

	securityTab := container.NewPadded(container.NewVBox(
		widget.NewLabel("Limit which remote actions and keys are allowed:"),
		container.NewGridWithColumns(2,
			container.NewVBox(
				widget.NewButton("Allowed Remote Actions…", func() {
					showAllowedCommandsDialog(w, updateRemoteActionsStatus)
				}),
				actionsCard,
			),
			container.NewVBox(
				widget.NewButton("Key Access Control…", func() {
					showKeyControlDialog(w, updateKeyControlStatus)
				}),
				keyAccessCard,
			),
		),
	))

	aboutObjs := []fyne.CanvasObject{
		widget.NewLabel("Bitfocus Listener is a remote-control WebSocket server."),
	}
	if runtime.GOOS == "darwin" {
		aboutObjs = append(aboutObjs,
			widget.NewSeparator(),
			widget.NewLabel("macOS may require Accessibility permission for keyboard and mouse control."),
			widget.NewButton("Open Accessibility Settings", openAccessibilitySettings),
		)
	}
	aboutTab := container.NewPadded(container.NewVBox(aboutObjs...))

	tabs := container.NewAppTabs(
		container.NewTabItem("Connection", connectionTab),
		container.NewTabItem("Security", securityTab),
		container.NewTabItem("About", aboutTab),
	)

	brandIcon := canvas.NewImageFromResource(appIcon)
	brandIcon.SetMinSize(fyne.NewSize(48, 48))
	brandIcon.FillMode = canvas.ImageFillContain
	brandTitle := canvas.NewText("Bitfocus Listener", theme.ForegroundColor())
	brandTitle.TextStyle = fyne.TextStyle{Bold: true}
	brandTitle.TextSize = 20
	brandTitle.Alignment = fyne.TextAlignCenter

	clientsCard := newStatusCard("Clients", connectionStatusText, connectionDetailText, connectionStatusBg)
	serverCard := newStatusCard("Server", listenStatusText, listenDetailText, listenStatusBg)

	headerGap := canvas.NewRectangle(color.Transparent)
	headerGap.SetMinSize(fyne.NewSize(0, 10))

	statusStrip := container.NewVBox(
		container.NewCenter(container.NewVBox(
			container.NewCenter(brandIcon),
			brandTitle,
		)),
		container.NewGridWithColumns(2, container.NewPadded(clientsCard), container.NewPadded(serverCard)),
		headerGap,
	)

	copyrightYear := time.Now().Year()
	if BUILDTIME > 0 {
		if y := time.Unix(BUILDTIME, 0).Year(); y >= 2020 {
			copyrightYear = y
		}
	}
	footerCopyright := canvas.NewText(fmt.Sprintf("Bitfocus AS © %d  ·  %s", copyrightYear, HumanVersion), theme.PlaceHolderColor())
	footerCopyright.Alignment = fyne.TextAlignCenter
	footerCopyright.TextSize = 11
	footer := container.NewVBox(
		container.NewCenter(trayHint),
		container.NewCenter(footerCopyright),
	)

	w.SetContent(container.NewPadded(container.NewBorder(
		statusStrip,
		container.NewPadded(footer),
		nil, nil,
		tabs,
	)))
	w.Resize(fyne.NewSize(640, 520))
	w.SetFixedSize(false)
	guiReady = true
	w.ShowAndRun()
}

func main() {
	// loadConfig must be called before starting the server (no mutex needed yet).
	if err := loadConfig(); err != nil {
		log.Fatal("Error loading config:", err)
	}
	if err := initAuditLogger(); err != nil {
		log.Fatal("Error initializing audit logger:", err)
	}
	auditLog("system", "STARTED", fmt.Sprintf("version=%s port=%s", HumanVersion, config.Port))
	// Start the server initially.
	startHTTPServer(config.Interface, config.Port, func(err error) {
		if err != nil {
			log.Printf("Initial binding error: %v", err)
		}
	})
	startGUI()
}
