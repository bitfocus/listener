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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-vgo/robotgo"
	"github.com/gorilla/websocket"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/process"

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

// trayIconSVG is the monochrome menu-bar/tray glyph. It is a macOS template
// image (shape carried entirely in the alpha channel) so the system can tint
// it for light/dark menu bars.
//
//go:embed tray_icon.svg
var trayIconSVG []byte

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
	// FileWriteRoots maps the folder names clients use in fileWrite to real
	// folders. Only the local GUI can change it.
	FileWriteRoots map[string]string `json:"file_write_roots,omitempty"`
}

var config Config
var configMu sync.RWMutex // protects config reads/writes
var configPath string
var storedPassword string
var connectionStatus string = "Disconnected"
var activeConnections int
var connMu sync.Mutex // protects activeConnections and connection status UI widgets

// GUI status widgets / hooks (nil until startGUI runs).
var clientsStatus *statusIndicator
var clientsDetail *widget.RichText
var serverStatus *statusIndicator
var bindErrorBanner *errorBanner
var refreshTrayMenu func(status string)

// guiStarted is set once the Fyne app exists; fyne.Do needs a current app.
var guiStarted atomic.Bool

// uiDo runs fn on the Fyne main goroutine. Before the GUI exists there is
// nothing to update, and startGUI renders the current state itself.
func uiDo(fn func()) {
	if guiStarted.Load() {
		fyne.Do(fn)
	}
}

var bindStatusMu sync.Mutex
var lastBindError error
var lastBindAddr string

// validAppleScriptName matches safe application/process names (alphanumeric, spaces, hyphens, dots).
var validAppleScriptName = regexp.MustCompile(`^[a-zA-Z0-9 .\-_]+$`)

// defaultCommands lists the command types that can be toggled.
var defaultCommands = []string{
	"keyPress", "keyCombinationPress",
	"keyDown", "keyUp", "osxKeyPressProcess", "osxAppleScript", "keyString", "shellRun",
	"fileOpen", "fileWrite", "mousePositionSet", "mousePositionGet", "mouseClick",
	"subscribe",
}

// commandGates maps helper commands to the toggle that controls them, so they
// are allowed exactly when that command is.
var commandGates = map[string]string{"fileWriteRootsGet": "fileWrite"}

// offByDefault lists commands that stay disabled even on a fresh install: the
// ones that can run arbitrary code or launch programs, and fileWrite.
var offByDefault = map[string]bool{"shellRun": true, "osxAppleScript": true, "fileOpen": true, "fileWrite": true}

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
		config.AllowedCommands = make(map[string]bool)
		for _, cmd := range defaultCommands {
			config.AllowedCommands[cmd] = !offByDefault[cmd]
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
			config.AllowedCommands[cmd] = !offByDefault[cmd]
		}
	}
	// A hand-edited config can't widen what fileWrite may write to.
	for name, dir := range config.FileWriteRoots {
		if !validRootName.MatchString(name) {
			log.Printf("Ignoring file write folder %q: invalid name", name)
			delete(config.FileWriteRoots, name)
		} else if _, err := validateWriteRoot(dir); err != nil {
			log.Printf("Ignoring file write folder %q: %v", name, err)
			delete(config.FileWriteRoots, name)
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
	dirty := false
	// Commands added after this config was written stay off until the user
	// enables them; record that explicitly rather than relying on a missing key.
	for _, cmd := range defaultCommands {
		if _, ok := config.AllowedCommands[cmd]; !ok {
			log.Printf("New command %s is disabled; enable it under Allowed Remote Actions", cmd)
			config.AllowedCommands[cmd] = false
			dirty = true
		}
	}
	// An empty password would make the expected digest MD5(salt), which any
	// client can compute from the challenge.
	if config.Password == "" {
		log.Println("Config has no password; generating a new one")
		config.Password = generatePassword()
		dirty = true
	}
	storedPassword = config.Password
	if dirty {
		return saveConfig(&config)
	}
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
	// Repeats counts identical entries folded into this one; see AuditLogger.Log.
	Repeats int
	Last    time.Time
}

func (e AuditEntry) String() string {
	line := fmt.Sprintf("%s [%s] %s %s", e.Time.Format(time.RFC3339), e.IP, e.Action, e.Detail)
	if e.Repeats > 0 {
		line += fmt.Sprintf(" (×%d)", e.Repeats+1)
	}
	return line
}

// auditRepeatWindow is how close together identical entries must be to fold
// into one line, so a button streaming commands doesn't flush the log.
const auditRepeatWindow = 10 * time.Second

// sameAuditEvent reports whether next repeats prev. mousePositionSet folds even
// when the coordinates differ, as mouse moves arrive as a stream.
func sameAuditEvent(prev, next AuditEntry) bool {
	if prev.IP != next.IP || prev.Action != next.Action || next.Time.Sub(prev.Last) > auditRepeatWindow {
		return false
	}
	return prev.Detail == next.Detail || next.Action == "mousePositionSet"
}

// sanitizeLogField escapes control characters so client-supplied text cannot
// forge extra lines or terminal sequences in the log file.
func sanitizeLogField(s string) string {
	if !strings.ContainsFunc(s, unicode.IsControl) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case unicode.IsControl(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// AuditLogger holds an in-memory ring buffer and writes to a log file.
type AuditLogger struct {
	mu       sync.Mutex
	entries  [auditRingSize]AuditEntry
	start    int // index of the oldest entry
	count    int
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

	a := &AuditLogger{filePath: logPath}

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
		// Put the file back and keep appending to it; the next write retries.
		log.Printf("Failed to rotate audit log: %v", err)
		if err := os.Rename(oldPath, a.filePath); err != nil {
			log.Printf("Failed to restore audit log after rotation error: %v", err)
		}
		return
	}
	a.file = f
	a.fileSize = 0
	old.Close()
}

func (a *AuditLogger) Log(ip, action, detail string) {
	now := time.Now()
	entry := AuditEntry{
		Time:   now,
		Last:   now,
		IP:     sanitizeLogField(ip),
		Action: sanitizeLogField(action),
		Detail: sanitizeLogField(detail),
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.count > 0 {
		last := &a.entries[(a.start+a.count-1)%auditRingSize]
		if sameAuditEvent(*last, entry) {
			last.Repeats++
			last.Last = now
			last.Detail = entry.Detail
			a.seq++
			return
		}
		// The file is append-only, so a folded run is summarised once it ends.
		if last.Repeats > 0 {
			a.writeLine(fmt.Sprintf("%s [%s] %s repeated %d more times, last %s",
				last.Last.Format(time.RFC3339), last.IP, last.Action, last.Repeats, last.Detail))
		}
	}

	if a.count < auditRingSize {
		a.entries[(a.start+a.count)%auditRingSize] = entry
		a.count++
	} else {
		a.entries[a.start] = entry
		a.start = (a.start + 1) % auditRingSize
	}
	a.seq++

	a.writeLine(entry.String())
}

// writeLine appends one line to the log file. Caller must hold a.mu.
func (a *AuditLogger) writeLine(line string) {
	if a.file == nil {
		return
	}
	n, err := a.file.WriteString(line + "\n")
	if err != nil {
		log.Printf("Failed to write audit log: %v", err)
		return
	}
	a.fileSize += int64(n)
	a.rotateIfNeeded()
}

// Snapshot returns a copy of the ring buffer contents along with the total
// number of entries ever logged. Once the ring is full its length stops
// changing, so the counter is the only reliable way for viewers to tell that
// the contents moved on.
func (a *AuditLogger) Snapshot() ([]AuditEntry, uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := make([]AuditEntry, a.count)
	for i := range result {
		result[i] = a.entries[(a.start+i)%auditRingSize]
	}
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
	s = sanitizeLogField(s)
	if len(s) <= maxLen {
		return s
	}
	// Cut on a rune boundary so the log stays valid UTF-8.
	cut := maxLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
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
	Root        string   `json:"root,omitempty"`    // fileWrite folder name
	Newline     string   `json:"newline,omitempty"` // fileWrite: lf, crlf or none
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

// Pre-auth frames only need to carry an auth digest; the cap stops an
// unauthenticated peer from forcing large allocations.
const maxUnauthMessageSize = 4 * 1024
const maxMessageSize = 1024 * 1024

var upgrader = websocket.Upgrader{CheckOrigin: checkOrigin}

// checkOrigin lets non-browser clients (no Origin header) through. Browser
// requests must be same-origin and addressed to an IP literal or one of this
// machine's own names; a DNS-rebinding page is same-origin under its own domain,
// so the host check is what stops it.
func checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || !strings.EqualFold(u.Host, r.Host) {
		return false
	}
	return isLocalHostName(r.Host)
}

func isLocalHostName(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if net.ParseIP(host) != nil || host == "localhost" {
		return true
	}
	name, err := os.Hostname()
	if err != nil {
		return false
	}
	base := strings.TrimSuffix(strings.ToLower(name), ".local")
	return host == base || host == base+".local"
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
	authMu        sync.Mutex
	authByIP      = map[string]*authBucket{}
	lastAuthPrune time.Time
)

// authBucketLocked returns ip's bucket, first dropping buckets idle for longer
// than a lockout (at most once a minute) so the map cannot grow without bound.
// Caller must hold authMu.
func authBucketLocked(ip string, now time.Time) *authBucket {
	if now.Sub(lastAuthPrune) > time.Minute {
		lastAuthPrune = now
		for k, b := range authByIP {
			if now.After(b.lockedUntil) && now.Sub(b.last) > authLockout {
				delete(authByIP, k)
			}
		}
	}
	b := authByIP[ip]
	if b == nil {
		b = &authBucket{}
		authByIP[ip] = b
	}
	return b
}

func checkAuthRateLimit(ip string) (ok bool, message string) {
	authMu.Lock()
	defer authMu.Unlock()
	now := time.Now()
	b := authBucketLocked(ip, now)
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
	now := time.Now()
	b := authBucketLocked(ip, now)
	b.attempts++
	b.last = now
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
	case "keyString":
		log.Printf("Received command: type=keyString chars=%d", utf8.RuneCountInString(cmd.Msg))
	case "osxAppleScript":
		log.Printf("Received command: type=%s msg=%q", cmd.Type, truncateForLog(cmd.Msg, 80))
	case "fileWrite":
		log.Printf("Received command: type=fileWrite root=%q path=%q bytes=%d", truncateForLog(cmd.Root, 40), truncateForLog(cmd.Path, 120), len(cmd.Msg))
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
	"f13": 105, "f14": 107, "f15": 113, "f16": 106,
	"f17": 64, "f18": 79, "f19": 80, "f20": 90,
	"num0": 82, "num1": 83, "num2": 84, "num3": 85, "num4": 86,
	"num5": 87, "num6": 88, "num7": 89, "num8": 91, "num9": 92,
	"num.": 65, "num+": 69, "num-": 78, "num*": 67, "num/": 75,
	"num_enter": 76, "num_equal": 81,
	// Mac keypads have Clear where PC keypads have Num Lock.
	"num_lock": 71,
	// Apple extended keyboards put Print Screen / Scroll Lock / Pause on
	// F13-F15 and Insert on Help.
	"print": 105, "printscreen": 105,
	"scroll": 107, "scrolllock": 107, "scroll_lock": 107,
	"break": 113, "pausebreak": 113, "pause_break": 113,
	"insert": 114, "help": 114,
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
	// robotgo only recognizes cmd/alt/ctrl/shift as modifier names and silently
	// ignores anything else, so the alias spellings clients send are mapped here.
	"win":     "cmd",
	"meta":    "cmd",
	"command": "cmd",
	"option":  "alt",
	"control": "ctrl",
	// Listener's key names that robotgo spells differently.
	"return":     "enter",
	"scroll":     "scroll_lock",
	"scrolllock": "scroll_lock",
	"break":      "pause_break",
	"pausebreak": "pause_break",
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

func normalizeRobotgoKeys(keys []string) []string {
	normalized := make([]string, len(keys))
	for i, key := range keys {
		normalized[i] = normalizeRobotgoKey(key)
	}
	return normalized
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

func runOsascript(script string) error {
	if err := exec.Command("osascript", "-e", script).Run(); err != nil {
		log.Printf("osascript failed: %v", err)
		return err
	}
	return nil
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
func darwinMediaTap(key string, modifiers []string) error {
	nx, ok := darwinNXMediaKeys[strings.ToLower(key)]
	if !ok {
		return fmt.Errorf("key %s is not supported on macOS", key)
	}
	postDarwinMediaKey(nx, modifiers)
	return nil
}

// tapKey sends a key press. On macOS this uses System Events / CoreGraphics media keys
// (not robotgo) to avoid CGO SIGTRAP crashes from background goroutines.
func tapKey(key string, modifiers ...string) error {
	if runtime.GOOS == "darwin" {
		if isDarwinMediaKey(key) {
			return darwinMediaTap(key, modifiers)
		}
		script, ok := darwinKeyTapScript(key, modifiers)
		if !ok {
			return fmt.Errorf("key %s is not supported on macOS", key)
		}
		return runOsascript(script)
	}
	mods := normalizeRobotgoKeys(modifiers)
	if vk, ok := virtualKeyCode(key); ok {
		var err error
		pressed := 0
		for _, mod := range mods {
			if err = robotgo.KeyDown(mod); err != nil {
				break
			}
			pressed++
		}
		if err == nil {
			sendVirtualKey(vk, true)
			sendVirtualKey(vk, false)
		}
		for i := pressed - 1; i >= 0; i-- {
			_ = robotgo.KeyUp(mods[i])
		}
		return err
	}
	return robotgo.KeyTap(normalizeRobotgoKey(key), toInterfaceSlice(mods)...)
}

// toggleKey sends key down/up. On macOS this uses System Events (osascript).
// Media keys have no meaningful hold state; a down is treated as a tap and up is ignored.
func toggleKey(key string, down bool) error {
	if runtime.GOOS == "darwin" {
		if isDarwinMediaKey(key) {
			if down {
				return darwinMediaTap(key, nil)
			}
			return nil
		}
		script, ok := darwinKeyToggleScript(key, down)
		if !ok {
			return fmt.Errorf("key %s is not supported on macOS", key)
		}
		return runOsascript(script)
	}
	if vk, ok := virtualKeyCode(key); ok {
		sendVirtualKey(vk, down)
		return nil
	}
	key = normalizeRobotgoKey(key)
	if down {
		return robotgo.KeyDown(key)
	}
	return robotgo.KeyUp(key)
}

func sendKeyError(client *Client, err error) {
	log.Printf("Key event failed: %v", err)
	auditLog(client.IP, "COMMAND_FAILED", err.Error())
	_ = client.writeJSON(map[string]interface{}{
		"type":    "error",
		"message": err.Error(),
	})
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

// keysAllowed reports whether key and all modifiers pass key access control,
// auditing and replying with an error for the first one that does not.
func keysAllowed(client *Client, cmdType, key string, modifiers []string) bool {
	if !isKeyAllowed(key) {
		log.Printf("Key %s is not allowed", key)
		auditLog(client.IP, "KEY_BLOCKED", fmt.Sprintf("%s key=%s", cmdType, key))
		_ = client.writeJSON(map[string]interface{}{
			"type":    "error",
			"message": "Key " + key + " is not allowed",
		})
		return false
	}
	for _, modifier := range modifiers {
		if !isKeyAllowed(modifier) {
			log.Printf("Modifier %s is not allowed", modifier)
			auditLog(client.IP, "KEY_BLOCKED", fmt.Sprintf("%s modifier=%s", cmdType, modifier))
			_ = client.writeJSON(map[string]interface{}{
				"type":    "error",
				"message": "Modifier " + modifier + " is not allowed",
			})
			return false
		}
	}
	return true
}

// Updated handleCommand with unified subscription handling.
func handleCommand(client *Client, cmd Command) {
	logCommand(cmd)
	if !client.authenticated.Load() {
		if cmd.Type == "auth" {
			expected := computeMD5(client.Salt + currentPassword())
			// Constant-time compare so response timing cannot leak how much of
			// the digest matched.
			matched := subtle.ConstantTimeCompare([]byte(cmd.Password), []byte(expected)) == 1
			// The rate limit gates failures only. Peers behind one IP share a
			// bucket, so letting a correct digest through means a misbehaving
			// neighbor cannot lock out a legitimate client; with 128-bit
			// generated passwords the extra guesses this allows are irrelevant.
			if !matched {
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
			}
			if matched {
				client.authenticated.Store(true)
				client.Conn.SetReadLimit(maxMessageSize)
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

	// Checked only after auth so unauthenticated peers can't probe which
	// commands are enabled or flood the audit log with BLOCKED entries.
	if cmd.Type != "auth" && cmd.Type != "unsubscribe" {
		gate, isHelper := commandGates[cmd.Type]
		if !isHelper {
			gate = cmd.Type
		}
		configMu.RLock()
		allowed, exists := config.AllowedCommands[gate]
		configMu.RUnlock()
		if !exists || !allowed {
			reason, message := "is disabled", "Command "+cmd.Type+" is disabled on the server"
			switch {
			case !exists && strings.HasPrefix(cmd.Type, "osx") && runtime.GOOS != "darwin":
				reason, message = "is not a known command", "Command "+cmd.Type+" is only supported on macOS"
			case !exists:
				reason, message = "is not a known command", "Command "+cmd.Type+" is not supported by this version of Listener"
			}
			log.Printf("Command %s %s", truncateForLog(cmd.Type, 60), reason)
			// Clients send helper commands on their own (e.g. on every connect), so a
			// refusal isn't something the user did and would only clutter the log.
			if !isHelper {
				auditLog(client.IP, "BLOCKED", fmt.Sprintf("command %s %s", truncateForLog(cmd.Type, 60), reason))
			}
			_ = client.writeJSON(map[string]interface{}{
				"type":    "error",
				"message": message,
			})
			return
		}
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
		if !keysAllowed(client, cmd.Type, cmd.Key, nil) {
			return
		}
		auditLog(client.IP, "keyPress", fmt.Sprintf("key=%s", cmd.Key))
		if err := tapKey(cmd.Key); err != nil {
			sendKeyError(client, err)
			return
		}
		log.Printf("Executed key tap: %v", cmd)

	case "keyCombinationPress":
		if !keysAllowed(client, cmd.Type, cmd.Key, cmd.Modifiers) {
			return
		}
		auditLog(client.IP, "keyCombinationPress", fmt.Sprintf("key=%s modifiers=%v", cmd.Key, cmd.Modifiers))
		if err := tapKey(cmd.Key, cmd.Modifiers...); err != nil {
			sendKeyError(client, err)
			return
		}
		log.Printf("Executed key tap with modifiers: %s + %v", cmd.Key, cmd.Modifiers)

	case "keyDown":
		if !keysAllowed(client, cmd.Type, cmd.Key, nil) {
			return
		}
		auditLog(client.IP, "keyDown", fmt.Sprintf("key=%s", cmd.Key))
		if err := toggleKey(cmd.Key, true); err != nil {
			sendKeyError(client, err)
			return
		}
		log.Printf("Key down: %s", cmd.Key)

	case "keyUp":
		if !keysAllowed(client, cmd.Type, cmd.Key, nil) {
			return
		}
		auditLog(client.IP, "keyUp", fmt.Sprintf("key=%s", cmd.Key))
		if err := toggleKey(cmd.Key, false); err != nil {
			sendKeyError(client, err)
			return
		}
		log.Printf("Key up: %s", cmd.Key)

	case "osxKeyPressProcess":
		if runtime.GOOS == "darwin" {
			// Validate processName to prevent AppleScript injection
			if !validAppleScriptName.MatchString(cmd.ProcessName) {
				log.Printf("Invalid process name rejected: %q", cmd.ProcessName)
				auditLog(client.IP, "BLOCKED", fmt.Sprintf("osxKeyPressProcess invalid process=%q", truncateForLog(cmd.ProcessName, 60)))
				client.writeJSON(map[string]interface{}{
					"type":    "error",
					"message": "Invalid process name",
				})
				return
			}
			if !keysAllowed(client, cmd.Type, cmd.Key, cmd.Modifiers) {
				return
			}
			// Validate key contains no AppleScript injection characters
			if strings.ContainsAny(cmd.Key, "\"\\") {
				log.Printf("Invalid key rejected: %q", cmd.Key)
				auditLog(client.IP, "BLOCKED", fmt.Sprintf("osxKeyPressProcess invalid key=%q", truncateForLog(cmd.Key, 60)))
				client.writeJSON(map[string]interface{}{
					"type":    "error",
					"message": "Invalid key value",
				})
				return
			}
			script, ok := darwinKeyTapScript(cmd.Key, cmd.Modifiers)
			if !ok {
				sendKeyError(client, fmt.Errorf("key %s is not supported on macOS", cmd.Key))
				return
			}
			auditLog(client.IP, "osxKeyPressProcess", fmt.Sprintf("process=%s key=%s modifiers=%v", cmd.ProcessName, cmd.Key, cmd.Modifiers))
			processName, clientIP := cmd.ProcessName, client.IP
			go func() {
				if err := runOsascript(fmt.Sprintf("tell application %q to activate", processName)); err != nil {
					auditLog(clientIP, "COMMAND_FAILED", fmt.Sprintf("osxKeyPressProcess activate %s: %v", processName, err))
				}
				if err := runOsascript(script); err != nil {
					auditLog(clientIP, "COMMAND_FAILED", fmt.Sprintf("osxKeyPressProcess key=%s: %v", cmd.Key, err))
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
			msg, clientIP := cmd.Msg, client.IP
			go func() {
				// osxAppleScript intentionally runs arbitrary operator-supplied AppleScript;
				// it requires authentication and can be disabled via AllowedCommands.
				// nosec
				if err := exec.Command("osascript", "-e", msg).Run(); err != nil {
					log.Printf("OSXAppleScript error: %v", err)
					auditLog(clientIP, "COMMAND_FAILED", fmt.Sprintf("osxAppleScript: %v", err))
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
		// Typed text is often a password, so only its length is ever logged.
		chars := utf8.RuneCountInString(cmd.Msg)
		auditLog(client.IP, "keyString", fmt.Sprintf("chars=%d", chars))
		typeString(cmd.Msg)
		log.Printf("Typed string: %d chars", chars)

	case "shellRun":
		// shellRun intentionally executes arbitrary operator-supplied shell commands;
		// it requires authentication and can be disabled via AllowedCommands. The
		// command injection it "enables" is the documented purpose of this feature.
		auditLog(client.IP, "shellRun", truncateForLog(cmd.Shell, 100))
		shell, clientIP := cmd.Shell, client.IP
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
				auditLog(clientIP, "COMMAND_FAILED", fmt.Sprintf("shellRun %s: %v", truncateForLog(shell, 60), err))
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
			auditLog(client.IP, "COMMAND_FAILED", fmt.Sprintf("fileOpen %s: %v", target, err))
		} else {
			go func() { _ = command.Wait() }()
		}
		log.Printf("Opened file: %s", target)

	case "fileWrite":
		rootName, written, n, err := writeToFile(cmd.Root, cmd.Path, cmd.Msg, cmd.Newline)
		if err != nil {
			log.Printf("fileWrite failed: %v", err)
			action := "COMMAND_FAILED"
			var rej *writeRejection
			if errors.As(err, &rej) {
				action = "BLOCKED"
			}
			auditLog(client.IP, action, fmt.Sprintf("fileWrite root=%s path=%s: %v", truncateForLog(rootName, 40), truncateForLog(cmd.Path, 120), err))
			_ = client.writeJSON(map[string]interface{}{
				"type":    "error",
				"message": "fileWrite: " + err.Error(),
			})
			return
		}
		auditLog(client.IP, "fileWrite", fmt.Sprintf("root=%s path=%s bytes=%d", truncateForLog(rootName, 40), truncateForLog(written, 120), n))

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

	case "fileWriteRootsGet":
		_ = client.writeJSON(fileWriteRootsMessage("fileWriteRootsGetResponse"))

	case "mousePositionGet":
		// Read-only and usually polled, so not audited.
		x, y := getMousePos()
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
		auditLog(client.IP, "BLOCKED", fmt.Sprintf("command %s is not a known command", truncateForLog(cmd.Type, 60)))
	}
}

// sendSysInfo pushes one metrics update. Returns false only when the WebSocket
// write fails (caller should end the subscription). Metric collection errors
// are logged and the subscription continues.
// sysInfoMaxAge is just under the 5s subscription tick, so subscribers share
// one sample per tick instead of each enumerating processes.
const sysInfoMaxAge = 4 * time.Second

var (
	sysInfoMu   sync.Mutex
	sysInfoLast SysInfo
	sysInfoAt   time.Time
)

// sharedSystemInfo samples at most once per sysInfoMaxAge. It also keeps
// gopsutil's cpu.Percent(0, …) meaningful: that measures since the previous
// call process-wide, so interleaved per-client calls would shrink the window.
func sharedSystemInfo() (SysInfo, error) {
	sysInfoMu.Lock()
	defer sysInfoMu.Unlock()
	if !sysInfoAt.IsZero() && time.Since(sysInfoAt) < sysInfoMaxAge {
		return sysInfoLast, nil
	}
	info, err := getSystemInfo()
	if err != nil {
		return info, err
	}
	sysInfoLast, sysInfoAt = info, time.Now()
	return info, nil
}

func sendSysInfo(client *Client) bool {
	sysInfo, err := sharedSystemInfo()
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

func wsHandler(w http.ResponseWriter, r *http.Request, gen uint64) {
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
	conn.SetReadLimit(maxUnauthMessageSize)
	registerClient(client)
	defer unregisterClient(client)
	// A rebind bumps bindGeneration before disconnectAllClients, and Shutdown
	// does not close hijacked connections. Checking after registering means a
	// session upgraded on the old listener is either caught by that disconnect
	// or sees the newer generation here.
	if !isCurrentBind(gen) {
		conn.Close()
		return
	}

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

// refreshConnectionStatus updates the status text based on activeConnections count.
// Must be called with connMu held.
func refreshConnectionStatus() {
	var status, summary, detail string
	level := statusIdle
	if activeConnections > 0 {
		status = fmt.Sprintf("Connected (%s)", plural(activeConnections, "client", "clients"))
		summary = fmt.Sprintf("%d connected", activeConnections)
		level = statusOK
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
		summary = "None connected"
	}
	connectionStatus = status
	uiDo(func() {
		if clientsStatus != nil {
			clientsStatus.Set(level, summary)
		}
		if clientsDetail != nil {
			setSubtleText(clientsDetail, detail)
		}
		if refreshTrayMenu != nil {
			refreshTrayMenu(status)
		}
	})
}

func setBindResult(addr string, err error) {
	bindStatusMu.Lock()
	lastBindAddr = addr
	lastBindError = err
	bindStatusMu.Unlock()
	refreshListenStatusUI()
}

func refreshListenStatusUI() {
	bindStatusMu.Lock()
	err := lastBindError
	addr := lastBindAddr
	bindStatusMu.Unlock()
	if addr == "" {
		configMu.RLock()
		addr = config.Interface + ":" + config.Port
		configMu.RUnlock()
	}
	uiDo(func() {
		if serverStatus == nil {
			return
		}
		if err != nil {
			serverStatus.Set(statusError, "Not listening")
			bindErrorBanner.Set(describeBindError(addr, err))
			bindErrorBanner.Show()
		} else {
			serverStatus.Set(statusOK, "Listening on "+addr)
			bindErrorBanner.Hide()
		}
	})
}

// describeBindError turns a listen failure into a headline and a next step the
// user can act on, instead of the raw Go error.
func describeBindError(addr string, err error) (string, string) {
	port := addr
	if _, p, splitErr := net.SplitHostPort(addr); splitErr == nil {
		port = p
	}
	switch {
	case errors.Is(err, errAddrInUse):
		return fmt.Sprintf("Port %s is already in use", port),
			"Another app, or another copy of Bitfocus Listener, is using this port. Close it or choose a different port below."
	case errors.Is(err, errAddrDenied):
		return fmt.Sprintf("Not allowed to use port %s", port),
			"Your system blocked access to this port. Choose a port above 1024 below."
	case errors.Is(err, errAddrNotAvail):
		return "Network interface unavailable",
			"The selected network interface is no longer available on this computer. Choose another interface below."
	}
	return "The server couldn't start", err.Error()
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

// revealInFileManager shows path selected in Finder / Explorer, or opens its
// folder elsewhere.
func revealInFileManager(path string) {
	if path == "" {
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", "-R", path)
	case "windows":
		cmd = exec.Command("explorer", "/select,"+path)
	default:
		cmd = exec.Command("xdg-open", filepath.Dir(path))
	}
	if err := cmd.Start(); err != nil {
		log.Printf("Failed to reveal %s: %v", path, err)
		return
	}
	go func() { _ = cmd.Wait() }()
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

func isCurrentBind(gen uint64) bool {
	serverMu.Lock()
	defer serverMu.Unlock()
	return gen == bindGeneration
}

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
		if !applied {
			return
		}
		if err != nil {
			auditLog("gui", "SERVER_RESTART_FAILED", fmt.Sprintf("bind=%s:%s error=%v", bindIP, port, err))
		} else {
			auditLog("gui", "SERVER_RESTARTED", fmt.Sprintf("bind=%s:%s", bindIP, port))
		}
		if errorCallback != nil {
			errorCallback(err)
		}
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
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		wsHandler(w, r, gen)
	})
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
			auditLog("system", "SERVER_ERROR", fmt.Sprintf("bind=%s error=%v", addr, serveErr))
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

const allInterfacesLabel = "All interfaces (0.0.0.0)"

func getInterfaceOptions() ([]string, map[string]string) {
	options := []string{allInterfacesLabel}
	mapping := map[string]string{allInterfacesLabel: "0.0.0.0"}
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
	"fileWrite":           "Create / Write File",
	"mousePositionSet":    "Set Mouse Position",
	"mousePositionGet":    "Get Mouse Position",
	"mouseClick":          "Mouse Click",
	"subscribe":           "Event Subscriptions",
}

// commandGroups orders the Allowed Remote Actions dialog. Commands missing
// from defaultCommands on this platform are skipped.
var commandGroups = []struct {
	name     string
	commands []string
}{
	{"Keyboard", []string{"keyPress", "keyCombinationPress", "keyDown", "keyUp", "keyString"}},
	{"Mouse", []string{"mouseClick", "mousePositionSet", "mousePositionGet"}},
	{"System", []string{"shellRun", "fileOpen", "fileWrite", "subscribe"}},
	{"macOS", []string{"osxKeyPressProcess", "osxAppleScript"}},
}

// groupColumns stacks groups into two independent columns, alternating left
// and right. A plain grid would give every row the tallest group's height.
func groupColumns(groups []fyne.CanvasObject) fyne.CanvasObject {
	left, right := container.NewVBox(), container.NewVBox()
	for i, g := range groups {
		if i%2 == 0 {
			left.Add(g)
		} else {
			right.Add(g)
		}
	}
	return container.NewGridWithColumns(2, left, right)
}

func showAllowedCommandsDialog(parent fyne.Window, updateStatusCallback func()) {
	// Snapshot the current state; config is mutated from the WebSocket goroutines.
	configMu.RLock()
	allowedNow := make(map[string]bool, len(config.AllowedCommands))
	for cmd, ok := range config.AllowedCommands {
		allowedNow[cmd] = ok
	}
	configMu.RUnlock()

	available := make(map[string]bool, len(defaultCommands))
	for _, cmd := range defaultCommands {
		available[cmd] = true
	}
	checkboxes := make(map[string]*keyCheck)
	// checkAll ticks or clears cbs through SetChecked, so group headers resync.
	checkAll := func(cbs []*keyCheck, on bool) {
		for _, cb := range cbs {
			cb.SetChecked(on)
		}
	}
	newGroup := func(name string, cmds []string) fyne.CanvasObject {
		// The group's "Allow all" mirrors its actions: ticked when all are on,
		// a dash when only some are. It is updated with setChecked so syncing
		// it doesn't fire its own OnChanged and flip every action.
		groupCheck := newKeyCheck("Allow all", nil)
		var members []*keyCheck
		syncGroup := func() {
			on := 0
			for _, cb := range members {
				if cb.Checked {
					on++
				}
			}
			setChecked(&groupCheck.Check, on == len(members))
			groupCheck.Partial = on > 0 && on < len(members)
			groupCheck.Refresh()
		}

		var objs []fyne.CanvasObject
		for _, cmd := range cmds {
			description, exists := commandDescriptions[cmd]
			if !exists {
				description = cmd
			}
			cb := newKeyCheck(description, func(bool) { syncGroup() })
			setChecked(&cb.Check, allowedNow[cmd])
			checkboxes[cmd] = cb
			members = append(members, cb)
			objs = append(objs, cb)
		}
		syncGroup()
		groupCheck.OnChanged = func(on bool) { checkAll(members, on) }
		return newSectionWithAction(name, groupCheck, objs...)
	}

	var groups []fyne.CanvasObject
	grouped := make(map[string]bool)
	for _, g := range commandGroups {
		var cmds []string
		for _, cmd := range g.commands {
			grouped[cmd] = true
			if available[cmd] {
				cmds = append(cmds, cmd)
			}
		}
		if len(cmds) > 0 {
			groups = append(groups, newGroup(g.name, cmds))
		}
	}
	var other []string
	for _, cmd := range defaultCommands {
		if !grouped[cmd] {
			other = append(other, cmd)
		}
	}
	if len(other) > 0 {
		groups = append(groups, newGroup("Other", other))
	}

	save := func() {
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
	}

	all := make([]*keyCheck, 0, len(checkboxes))
	for _, cb := range checkboxes {
		all = append(all, cb)
	}
	allowAll := newKeyButton("Allow all", func() { checkAll(all, true) })
	allowAll.Importance = widget.LowImportance
	disableAll := newKeyButton("Disable all", func() { checkAll(all, false) })
	disableAll.Importance = widget.LowImportance
	intro := widget.NewLabel("Choose which commands clients can run.")
	intro.Truncation = fyne.TextTruncateEllipsis
	content := container.NewBorder(
		container.NewBorder(nil, nil, nil, container.NewHBox(allowAll, disableAll), intro),
		nil, nil, nil,
		groupColumns(groups),
	)

	d := newSheet(parent, "Remote Actions", "",
		content,
		sheetButton{label: "Cancel"},
		sheetButton{label: "Save", importance: widget.HighImportance, onTap: save},
	)
	d.Resize(fyne.NewSize(560, 0))
	d.Show()
}

// -------------------------
// Key Control Configuration
// -------------------------

// Key categories for easier selection
var keyCategories = map[string][]string{
	"Navigation": {"up", "down", "left", "right", "home", "end", "pageup", "pagedown", "tab", "backspace", "delete", "insert"},
	"Function":   {"f1", "f2", "f3", "f4", "f5", "f6", "f7", "f8", "f9", "f10", "f11", "f12", "f13", "f14", "f15", "f16", "f17", "f18", "f19", "f20", "f21", "f22", "f23", "f24"},
	"Media":      {"play", "pause", "stop", "next", "previous", "volumeup", "volumedown", "mute"},
	"System":     {"esc", "enter", "space", "capslock", "menu", "print", "scroll", "break"},
	"Numpad":     {"num0", "num1", "num2", "num3", "num4", "num5", "num6", "num7", "num8", "num9", "num.", "num+", "num-", "num*", "num/", "num_enter", "num_equal", "num_lock"},
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
	"capslock":   "Caps Lock",
	"menu":       "Menu / Context",
	"print":      "Print Screen",
	"scroll":     "Scroll Lock",
	"pause":      "Pause",
	"break":      "Pause/Break",
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
	"num0":       "Numpad 0",
	"num1":       "Numpad 1",
	"num2":       "Numpad 2",
	"num3":       "Numpad 3",
	"num4":       "Numpad 4",
	"num5":       "Numpad 5",
	"num6":       "Numpad 6",
	"num7":       "Numpad 7",
	"num8":       "Numpad 8",
	"num9":       "Numpad 9",
	"num.":       "Numpad .",
	"num+":       "Numpad +",
	"num-":       "Numpad -",
	"num*":       "Numpad *",
	"num/":       "Numpad /",
	"num_enter":  "Numpad Enter",
	"num_equal":  "Numpad =",
	"num_lock":   "Num Lock",
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
	if n, err := strconv.Atoi(strings.TrimPrefix(key, "f")); err == nil && key[0] == 'f' && n > 0 {
		return "F" + key[1:]
	}
	return key
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
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
	case "pausebreak", "pause_break":
		return "break"
	case "printscreen":
		return "print"
	case "scrolllock", "scroll_lock":
		return "scroll"
	case "caps_lock":
		return "capslock"
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

	categoryCheckboxes := make(map[string]*keyCheck)
	keyCheckboxes := make(map[string]*keyCheck)
	// keyAllowed holds each key's own setting. It is kept separately from the
	// checkbox because a key is shown checked and locked while its whole
	// category is allowed, and must revert when the category is unticked.
	keyAllowed := make(map[string]bool)
	// searchText is each key's lowercased display name and id, built once
	// rather than on every keystroke in the search field.
	searchText := make(map[string]string)

	countText := newSubtleText("")
	updateSelectionCount := func() {
		nCats, nKeys := 0, 0
		for _, cb := range categoryCheckboxes {
			if cb.Checked {
				nCats++
			}
		}
		for _, ok := range keyAllowed {
			if ok {
				nKeys++
			}
		}
		setSubtleText(countText, fmt.Sprintf("Allowed: %s and %s",
			plural(nCats, "category", "categories"), plural(nKeys, "individual key", "individual keys")))
	}

	sortedCategories := make([]string, 0, len(keyCategories))
	for category := range keyCategories {
		sortedCategories = append(sortedCategories, category)
	}
	sort.Strings(sortedCategories)

	type categoryView struct {
		row      fyne.CanvasObject
		sep      fyne.CanvasObject
		keys     fyne.CanvasObject
		toggle   *tapRow
		expanded bool
	}
	views := make(map[string]*categoryView)
	// Showing or hiding rows doesn't re-lay out the scroll content by itself.
	var relayout func()
	// showKeys only changes what is drawn; setExpanded also records the
	// user's choice, which a search temporarily overrides and clearing it
	// restores.
	showKeys := func(v *categoryView, open bool) {
		if open == v.keys.Visible() {
			return
		}
		if open {
			v.toggle.SetIcon(theme.MenuDropDownIcon())
			v.keys.Show()
		} else {
			v.toggle.SetIcon(theme.MenuExpandIcon())
			v.keys.Hide()
		}
	}
	setExpanded := func(v *categoryView, open bool) {
		v.expanded = open
		showKeys(v, open)
	}
	applyCategoryLock := func(cat string) {
		whole := categoryCheckboxes[cat].Checked
		for _, k := range keyCategories[cat] {
			cb := keyCheckboxes[k]
			if whole {
				setChecked(&cb.Check, true)
				cb.Disable()
			} else {
				setChecked(&cb.Check, keyAllowed[k])
				cb.Enable()
			}
		}
	}

	var categoryRows []fyne.CanvasObject
	for i, cat := range sortedCategories {
		keys := keyCategories[cat]
		var keyChecks []fyne.CanvasObject
		for _, k := range keys {
			keyAllowed[k] = keysNow[k]
			searchText[k] = strings.ToLower(getKeyDisplayName(k) + " " + k)
			cb := newKeyCheck(getKeyDisplayName(k), func(on bool) {
				keyAllowed[k] = on
				updateSelectionCount()
			})
			keyCheckboxes[k] = cb
			keyChecks = append(keyChecks, cb)
		}

		catCheck := newKeyCheck("Allow all", func(bool) {
			applyCategoryLock(cat)
			updateSelectionCount()
		})
		setChecked(&catCheck.Check, categoriesNow[cat])
		categoryCheckboxes[cat] = catCheck
		applyCategoryLock(cat)

		v := &categoryView{}
		// The name row expands the key list; "Allow all" sits apart so the two
		// actions can't be confused.
		count := newSubtleText(fmt.Sprintf("%d keys", len(keys)))
		v.toggle = newTapRow(cat, container.NewCenter(count), theme.MenuExpandIcon(), func() {
			setExpanded(v, !v.keys.Visible())
			relayout()
		})
		header := container.NewBorder(nil, nil, nil, catCheck, v.toggle)
		v.keys = container.New(layout.NewCustomPaddedLayout(0, theme.Padding(), 3*theme.Padding(), 0),
			container.NewGridWithColumns(3, keyChecks...))
		v.row = container.NewVBox(header, v.keys)
		setExpanded(v, false)
		views[cat] = v

		if i > 0 {
			v.sep = widget.NewSeparator()
			categoryRows = append(categoryRows, v.sep)
		}
		categoryRows = append(categoryRows, v.row)
	}
	updateSelectionCount()

	// Filtering shows only matching keys and expands the categories that have any.
	searchEntry := newSearchEntry("Find a key…")
	searchEntry.OnChanged = func(filter string) {
		filter = strings.ToLower(strings.TrimSpace(filter))
		for _, cat := range sortedCategories {
			v := views[cat]
			matches := 0
			for _, k := range keyCategories[cat] {
				cb := keyCheckboxes[k]
				if filter == "" || strings.Contains(searchText[k], filter) {
					cb.Show()
					matches++
				} else {
					cb.Hide()
				}
			}
			if filter == "" {
				v.row.Show()
				showKeys(v, v.expanded)
			} else if matches > 0 || strings.Contains(strings.ToLower(cat), filter) {
				v.row.Show()
				showKeys(v, matches > 0)
			} else {
				v.row.Hide()
			}
		}
		// A separator only belongs between two visible rows.
		seenVisible := false
		for _, cat := range sortedCategories {
			v := views[cat]
			if v.sep != nil {
				if seenVisible && v.row.Visible() {
					v.sep.Show()
				} else {
					v.sep.Hide()
				}
			}
			if v.row.Visible() {
				seenVisible = true
			}
		}
		relayout()
	}

	// Locked keys are disabled checkboxes; this one override keeps them
	// readable (see readableDisabledTheme). Nothing else in it is disabled.
	categories := container.NewThemeOverride(newSection("Categories", categoryRows...),
		readableDisabledTheme{fyne.CurrentApp().Settings().Theme()})
	restrictedSection := container.NewVBox(searchEntry, categories, countText)

	const fullAccessOption, restrictedOption = "All keys", "Only selected keys"
	modeRadio := widget.NewRadioGroup([]string{fullAccessOption, restrictedOption}, func(selected string) {
		if selected == fullAccessOption {
			selectedMode = "full_access"
			restrictedSection.Hide()
		} else {
			selectedMode = "restricted"
			restrictedSection.Show()
		}
		relayout()
	})
	modeRadio.Horizontal = true
	modeRadio.Required = true

	content := container.NewVBox(newSection("Allow", modeRadio), restrictedSection)
	relayout = content.Refresh
	if selectedMode == "full_access" {
		modeRadio.SetSelected(fullAccessOption)
	} else {
		modeRadio.SetSelected(restrictedOption)
	}

	save := func() {
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
		// Only allowed keys are stored; a missing key reads as not allowed, so
		// the config doesn't fill with a false entry for every known key.
		for key, on := range keyAllowed {
			if config.AllowedKeys[key] != on {
				action := "disabled"
				if on {
					action = "enabled"
				}
				changes = append(changes, fmt.Sprintf("key:%s=%s", getKeyDisplayName(key), action))
			}
			if on {
				config.AllowedKeys[key] = true
			} else {
				delete(config.AllowedKeys, key)
			}
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
	}

	d := newSheet(parent, "Key Access", "Choose which keys remote clients are allowed to press.",
		container.NewVScroll(content),
		sheetButton{label: "Cancel"},
		sheetButton{label: "Save", importance: widget.HighImportance, onTap: save},
	)
	// Escape in the search field clears it first; with it empty, Escape
	// falls through to the sheet and closes it.
	searchEntry.OnEscape = func() bool {
		if searchEntry.Text == "" {
			return false
		}
		searchEntry.SetText("")
		return true
	}
	d.Resize(fyne.NewSize(600, 640))
	d.Show()
}

func showActivityLogDialog(parent fyne.Window) {
	// Each entry is stacked over two lines so it stays readable at the
	// settings window's width: time and source above, the event below.
	type logRow struct{ meta, action, detail, full string }
	var lines []logRow
	logList := widget.NewList(
		func() int { return len(lines) },
		func() fyne.CanvasObject {
			meta := newSubtleText("")
			event := widget.NewRichText(
				&widget.TextSegment{Style: widget.RichTextStyle{TextStyle: fyne.TextStyle{Bold: true}, Inline: true}},
				&widget.TextSegment{Style: widget.RichTextStyle{Inline: true}},
			)
			event.Truncation = fyne.TextTruncateEllipsis
			return container.New(layout.NewCustomPaddedLayout(theme.Padding(), theme.Padding(), 0, 0),
				container.New(layout.NewCustomPaddedVBoxLayout(0), meta, event))
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			row := lines[i]
			objs := o.(*fyne.Container).Objects[0].(*fyne.Container).Objects
			setSubtleText(objs[0].(*widget.RichText), row.meta)
			event := objs[1].(*widget.RichText)
			event.Segments[0].(*widget.TextSegment).Text = row.action
			event.Segments[1].(*widget.TextSegment).Text = row.detail
			event.Refresh()
		},
	)
	const copyHintText = "Click a line to copy it"
	copyHint := newSubtleText(copyHintText)
	// Selecting a row copies the full line. Unselect immediately so the same
	// row can be copied again and no stale highlight survives a refresh.
	logList.OnSelected = func(i widget.ListItemID) {
		if i < len(lines) {
			parent.Clipboard().SetContent(lines[i].full)
			flash(func() { setSubtleText(copyHint, "Copied to clipboard") },
				func() { setSubtleText(copyHint, copyHintText) })
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
			source := e.IP
			switch source {
			case "gui":
				source = "Settings"
			case "system":
				source = "System"
			}
			detail := ""
			if e.Detail != "" {
				detail = "  " + e.Detail
			}
			if e.Repeats > 0 {
				detail += fmt.Sprintf("  ×%d", e.Repeats+1)
			}
			next = append(next, logRow{
				meta:   e.Time.Format("Jan 2, 15:04:05") + "  ·  " + source,
				action: e.Action,
				detail: detail,
				full:   e.String(),
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
				fyne.Do(refreshLog)
			case <-done:
				return
			}
		}
	}()

	logFilePath := ""
	if audit != nil {
		logFilePath = audit.filePath
	}
	copyPathBtn := newCopyButton(parent.Clipboard(), func() string { return logFilePath })
	revealBtn := newIconButton(theme.FolderOpenIcon(), func() { revealInFileManager(logFilePath) })
	pathText := newSubtleText(logFilePath)
	subtleStyle(pathText).TextStyle = fyne.TextStyle{Monospace: true}
	subtleStyle(pathText).Inline = false
	pathText.Wrapping = fyne.TextWrapBreak

	content := container.NewBorder(
		copyHint,
		container.NewBorder(nil, nil, nil, container.NewHBox(revealBtn, copyPathBtn), pathText),
		nil, nil,
		newCard(container.NewThemeOverride(logList, compactTheme{fyne.CurrentApp().Settings().Theme()})),
	)

	logDialog := newSheet(parent, "Activity Log", "", content,
		sheetButton{label: "Done", importance: widget.HighImportance},
	)
	logDialog.SetOnClosed(stopRefresh)
	logDialog.Resize(fyne.NewSize(760, 560))
	logDialog.Show()
}

func startGUI() {
	buildMainWindow(app.New()).ShowAndRun()
	auditLog("system", "STOPPED", "")
}

func buildMainWindow(myApp fyne.App) fyne.Window {
	myApp.Settings().SetTheme(newListenerTheme())
	guiStarted.Store(true)
	appIcon := fyne.NewStaticResource("icon.png", iconPNG)
	myApp.SetIcon(appIcon)
	w := myApp.NewWindow("Bitfocus Listener")

	// Ignore bind-change handlers until the window is fully wired. Fyne Select
	// calls OnChanged from SetSelected, which previously restarted the server
	// on every launch and dropped WebSocket connections.
	guiReady := false
	// No Validator: Fyne would show a permanent ✓ beside a valid port. Range
	// errors show in portError below; bind failures in the error banner.
	portEntry := newDigitEntry(5)
	portEntry.SetText(config.Port)
	portError := widget.NewLabel("Enter a port from 1 to 65535")
	portError.Importance = widget.DangerImportance
	portError.SizeName = theme.SizeNameCaptionText
	portError.Hide()

	restartWithCurrentBind := func() {
		configMu.RLock()
		iface, port := config.Interface, config.Port
		configMu.RUnlock()
		restartServer(iface, port, nil)
	}

	bindErrorBanner = newErrorBanner()
	bindErrorBanner.Hide()

	connMu.Lock()
	clientsStatus = newStatusIndicator(statusIdle, "None connected")
	clientsDetail = newSubtleText("")
	serverStatus = newStatusIndicator(statusOK, "Listening")
	refreshConnectionStatus()
	connMu.Unlock()
	refreshListenStatusUI()

	trayHint := "Quit from the window close button when tray is unavailable."
	trayEnabled := false
	var deskApp desktop.App
	if desk, ok := myApp.(desktop.App); ok {
		trayEnabled = true
		deskApp = desk
		desk.SetSystemTrayIcon(trayIconResource(appIcon))
		trayHint = "Keeps running in the menu bar when this window is closed."
		if runtime.GOOS == "windows" {
			trayHint = "Keeps running in the system tray when this window is closed."
		}
	}

	buildTrayMenu := func(status string) {
		if deskApp == nil {
			return
		}
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
	connMu.Lock()
	initialStatus := connectionStatus
	connMu.Unlock()
	buildTrayMenu(initialStatus)

	w.SetCloseIntercept(func() {
		if trayEnabled {
			w.Hide()
			return
		}
		myApp.Quit()
	})
	var openSize fyne.Size
	myApp.Lifecycle().SetOnStarted(func() {
		hideDockIcon()
		applyWindowChrome(w)
		// The content view now spans the old title bar, so re-apply the size.
		w.Resize(openSize)
	})

	targetHostLabel := widget.NewLabel(websocketTargetHost())
	targetHostLabel.TextStyle = fyne.TextStyle{Monospace: true}

	options, ifaceMapping := getInterfaceOptions()
	initialIface := allInterfacesLabel
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
		targetHostLabel.SetText(websocketTargetHost())
		restartServer(iface, port, nil)
	})
	ifaceSelect.SetSelected(initialIface)

	parsePort := func(text string) (string, bool) {
		n, err := strconv.Atoi(text)
		if err != nil || n < 1 || n > 65535 {
			return "", false
		}
		return strconv.Itoa(n), true
	}
	currentPort := func() string {
		configMu.RLock()
		defer configMu.RUnlock()
		return config.Port
	}

	// A port change restarts the server and drops every client, so edits are
	// held until the user presses Apply (or Return) rather than applied as typed.
	var applyPortBtn *widget.Button
	applyPort := func() {
		newPort, ok := parsePort(portEntry.Text)
		if !ok {
			return
		}
		configMu.Lock()
		if config.Port == newPort {
			configMu.Unlock()
			applyPortBtn.Hide()
			return
		}
		// Roll back on a failed save so config keeps matching the running
		// server and Apply stays available to retry.
		oldPort := config.Port
		config.Port = newPort
		if err := saveConfigLocked(&config); err != nil {
			config.Port = oldPort
			configMu.Unlock()
			dialog.ShowError(err, w)
			return
		}
		applyPortBtn.Hide()
		iface, port := config.Interface, config.Port
		configMu.Unlock()
		portEntry.SetText(newPort)
		auditLog("gui", "PORT_CHANGED", fmt.Sprintf("port=%s", newPort))
		restartServer(iface, port, nil)
	}
	applyPortBtn = widget.NewButton("Apply", applyPort)
	applyPortBtn.Importance = widget.HighImportance
	applyPortBtn.Hide()

	portEntry.OnSubmitted = func(string) { applyPort() }
	// Leaving the field with an unusable value puts back the port in use; a
	// valid pending edit is kept so it can still be applied.
	portEntry.OnFocusLost = func() {
		if _, ok := parsePort(portEntry.Text); !ok {
			portEntry.SetText(currentPort())
		}
	}
	portEntry.OnChanged = func(text string) {
		newPort, ok := parsePort(text)
		if ok {
			portError.Hide()
		} else {
			portError.Show()
		}
		if ok && newPort != currentPort() {
			applyPortBtn.Show()
		} else {
			applyPortBtn.Hide()
		}
	}

	actionsStatus := widget.NewLabel("")
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
			actionsStatus.SetText("None enabled")
		case enabled == total:
			actionsStatus.SetText(fmt.Sprintf("All %d enabled", total))
		default:
			actionsStatus.SetText(fmt.Sprintf("%d of %d enabled", enabled, total))
		}
	}
	updateRemoteActionsStatus()

	foldersStatus := widget.NewLabel("")
	updateFoldersStatus := func() { foldersStatus.SetText(fileFoldersStatus()) }
	updateFoldersStatus()

	keyAccessStatus := widget.NewLabel("")
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
		switch {
		case mode == "full_access":
			keyAccessStatus.SetText("All keys")
		case nCats == 0 && nKeys == 0:
			keyAccessStatus.SetText("No keys")
		default:
			keyAccessStatus.SetText(plural(nCats, "category", "categories") + ", " + plural(nKeys, "key", "keys"))
		}
	}
	updateKeyControlStatus()

	// Password is generate-only, so it is shown as text rather than an Entry.
	passwordMasked := true
	passwordLabel := widget.NewLabel("")
	passwordLabel.TextStyle = fyne.TextStyle{Monospace: true}
	passwordLabel.Truncation = fyne.TextTruncateEllipsis
	refreshPasswordDisplay := func() {
		pwd := currentPassword()
		if passwordMasked {
			passwordLabel.SetText(strings.Repeat("•", len(pwd)))
		} else {
			passwordLabel.SetText(pwd)
		}
	}
	refreshPasswordDisplay()

	var showHideBtn *keyButton
	showHideBtn = newIconButton(theme.VisibilityIcon(), func() {
		passwordMasked = !passwordMasked
		if passwordMasked {
			showHideBtn.SetIcon(theme.VisibilityIcon())
		} else {
			showHideBtn.SetIcon(theme.VisibilityOffIcon())
		}
		refreshPasswordDisplay()
	})

	copyPasswordBtn := newCopyButton(w.Clipboard(), currentPassword)

	generateBtn := newIconButton(theme.ViewRefreshIcon(), func() {
		d := newSheet(w, "Generate a new password?",
			"All connected clients will be disconnected and must reconnect with the new password.",
			nil,
			sheetButton{label: "Cancel"},
			sheetButton{label: "Generate Password", importance: widget.DangerImportance, onTap: func() {
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
			}},
		)
		d.Resize(fyne.NewSize(400, 0))
		d.Show()
	})

	copyHostBtn := newCopyButton(w.Clipboard(), websocketTargetHost)

	statusSection := newSection("Status",
		container.NewBorder(nil, nil, widget.NewLabel("Server"), serverStatus),
		widget.NewSeparator(),
		container.NewBorder(nil, nil, widget.NewLabel("Clients"),
			container.NewHBox(container.NewCenter(clientsDetail), clientsStatus)),
	)

	connectionSection := newSection("Connection",
		container.New(layout.NewFormLayout(),
			widget.NewLabel("Interface"), ifaceSelect,
			widget.NewLabel("Port"), container.NewVBox(container.NewBorder(nil, nil, nil, applyPortBtn, portEntry), portError),
			widget.NewLabel("Password"), container.NewBorder(nil, nil, nil,
				container.NewHBox(showHideBtn, copyPasswordBtn, generateBtn), passwordLabel),
			widget.NewLabel("Target host"), container.NewBorder(nil, nil, nil, copyHostBtn, targetHostLabel),
		),
	)

	securitySection := newSection("Security",
		newTapRow("Remote actions", actionsStatus, nil, func() {
			showAllowedCommandsDialog(w, func() {
				updateRemoteActionsStatus()
				// fileWrite may have just been turned on.
				broadcastFileWriteRoots()
			})
		}),
		widget.NewSeparator(),
		newTapRow("Allowed write file paths", foldersStatus, nil, func() {
			showFileFoldersDialog(w, updateFoldersStatus)
		}),
		widget.NewSeparator(),
		newTapRow("Key access", keyAccessStatus, nil, func() {
			showKeyControlDialog(w, updateKeyControlStatus)
		}),
	)

	generalRows := []fyne.CanvasObject{
		newTapRow("Activity log", nil, nil, func() { showActivityLogDialog(w) }),
		widget.NewSeparator(),
		newTapRow("Restart server", nil, theme.MediaReplayIcon(), restartWithCurrentBind),
	}
	if runtime.GOOS == "darwin" {
		accessStatus := newStatusIndicator(statusIdle, "")
		refreshAccessStatus := func() {
			if accessibilityTrusted() {
				accessStatus.Set(statusOK, "Granted")
			} else {
				accessStatus.Set(statusError, "Not granted")
			}
		}
		refreshAccessStatus()
		// Permission is granted in System Settings, so re-check when the user comes back.
		myApp.Lifecycle().SetOnEnteredForeground(refreshAccessStatus)
		generalRows = append(generalRows,
			widget.NewSeparator(),
			newTapRow("Accessibility permission", accessStatus, nil, openAccessibilitySettings),
		)
	}
	generalSection := newSection("General", generalRows...)

	copyrightYear := time.Now().Year()
	if BUILDTIME > 0 {
		if y := time.Unix(BUILDTIME, 0).Year(); y >= 2020 {
			copyrightYear = y
		}
	}
	footerText := newSubtleText(fmt.Sprintf("Bitfocus AS © %d  ·  %s\n%s", copyrightYear, HumanVersion, trayHint))
	subtleStyle(footerText).Alignment = fyne.TextAlignCenter
	subtleStyle(footerText).Inline = false

	page := container.NewVBox(bindErrorBanner, statusSection, connectionSection, securitySection, generalSection)
	margin := 2 * theme.Padding()

	// The divider only appears once content scrolls under the header, like
	// macOS unified title bars. The spacer keeps its height reserved so the
	// layout doesn't shift when it toggles.
	divider := widget.NewSeparator()
	divider.Hide()
	dividerSpace := canvas.NewRectangle(color.Transparent)
	dividerSpace.SetMinSize(fyne.NewSize(0, theme.SeparatorThicknessSize()))
	dividerRow := container.NewStack(dividerSpace, divider)

	// On macOS the header fills the unified title bar, centred between the
	// traffic lights. Elsewhere the native title bar already shows the icon
	// and name, so a header would only repeat it.
	var top fyne.CanvasObject = container.New(layout.NewCustomPaddedLayout(margin, 0, 0, 0), dividerRow)
	if barHeight := titlebarInsets(); barHeight > 0 {
		brandIcon := canvas.NewImageFromResource(appIcon)
		brandIcon.SetMinSize(fyne.NewSize(32, 32))
		brandIcon.FillMode = canvas.ImageFillContain
		brandTitle := widget.NewLabel("Bitfocus Listener")
		brandTitle.TextStyle = fyne.TextStyle{Bold: true}
		headerRow := container.NewCenter(container.NewHBox(container.NewCenter(brandIcon), brandTitle))
		vInset := fyne.Max((barHeight-headerRow.MinSize().Height)/2, 0)
		header := container.New(layout.NewCustomPaddedLayout(vInset, vInset, margin, margin), headerRow)
		top = container.NewVBox(header, dividerRow)
	}

	body := container.New(layout.NewCustomPaddedLayout(0, 0, margin, margin), page)
	scroll := container.NewVScroll(body)
	syncDivider := func() {
		if scroll.Offset.Y > 0 {
			divider.Show()
		} else {
			divider.Hide()
		}
	}
	scroll.OnScrolled = func(fyne.Position) { syncDivider() }
	// Growing the window clamps the offset back to 0 without OnScrolled, so
	// re-check after every layout too.
	scrollArea := container.New(fillThenLayout{after: syncDivider}, scroll)
	footer := container.New(layout.NewCustomPaddedLayout(0, margin, margin, margin), footerText)
	w.SetPadded(false)
	w.SetContent(container.NewBorder(top, footer, nil, nil, scrollArea))
	// The scroll has no useful min height, so open tall enough to show every
	// section; Border adds one padding between each of its regions.
	openSize = fyne.NewSize(560, top.MinSize().Height+body.MinSize().Height+footer.MinSize().Height+2*theme.Padding())
	w.Resize(openSize)
	w.SetFixedSize(false)
	guiReady = true
	return w
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
			auditLog("system", "SERVER_START_FAILED", fmt.Sprintf("bind=%s:%s error=%v", config.Interface, config.Port, err))
		}
	})
	startGUI()
}
