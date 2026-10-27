package main

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
)

const (
	maxWriteBytes    = 64 * 1024
	maxWriteFileSize = 50 * 1024 * 1024
)

// fileWriteMu serializes appends so concurrent button presses can't interleave
// their text; O_APPEND alone isn't atomic for every write size or on Windows.
var fileWriteMu sync.Mutex

// allowedWriteExts keeps fileWrite to plain data files. Even inside a chosen
// folder, nothing that a shell, Finder or Explorer would execute can be written.
var allowedWriteExts = map[string]bool{
	".txt": true, ".log": true, ".csv": true, ".tsv": true, ".json": true, ".md": true,
	".jsonl": true, ".ndjson": true, ".srt": true, ".vtt": true, ".edl": true, ".xml": true,
	".yaml": true, ".yml": true,
}

// maxRootNameLen keeps folder names short enough to fit the settings column.
const maxRootNameLen = 16

var validRootName = regexp.MustCompile(fmt.Sprintf(`^[A-Za-z0-9_-]{1,%d}$`, maxRootNameLen))

// windowsReserved are device names Windows refuses as a file name, with or
// without an extension. They are rejected on every platform so a button
// behaves the same against a Mac and a Windows Listener.
var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true, "CONIN$": true, "CONOUT$": true,
	"COM0": true, "COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT0": true, "LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
	"COM¹": true, "COM²": true, "COM³": true, "LPT¹": true, "LPT²": true, "LPT³": true,
}

// writeRejection marks an error caused by the request itself (bad path, unknown
// folder) rather than by the filesystem, so it is audited as BLOCKED.
type writeRejection struct{ msg string }

func (e *writeRejection) Error() string { return e.msg }

func rejectf(format string, args ...any) error {
	return &writeRejection{fmt.Sprintf(format, args...)}
}

// sanitizeWritePath turns a client-supplied relative path into a portable one
// using "/" separators. Characters Windows forbids in names are replaced with
// "-" (Companion time variables contain ":"), so the same input gives the same
// file name on every platform. Escapes are also blocked by os.Root; rejecting
// them here gives the client a clearer error.
func sanitizeWritePath(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", rejectf("file path is empty")
	}
	for _, r := range rel {
		if r < 0x20 || r == 0x7f {
			return "", rejectf("file path contains control characters")
		}
	}
	rel = strings.ReplaceAll(rel, `\`, "/")
	if strings.HasPrefix(rel, "/") || (len(rel) >= 2 && rel[1] == ':' && isASCIILetter(rel[0])) {
		return "", rejectf("file path must be relative to the folder")
	}
	var parts []string
	for _, p := range strings.Split(rel, "/") {
		switch p {
		case "", ".":
			continue
		case "..":
			return "", rejectf("file path may not contain '..'")
		}
		p = strings.Map(func(r rune) rune {
			if strings.ContainsRune(`<>:"|?*`, r) {
				return '-'
			}
			return r
		}, p)
		// Windows silently strips trailing dots and spaces, which would make two
		// different inputs land on the same file.
		p = strings.TrimRight(p, ". ")
		if p == "" {
			return "", rejectf("file path has an empty name component")
		}
		stem, _, _ := strings.Cut(p, ".")
		if windowsReserved[strings.ToUpper(strings.TrimSpace(stem))] {
			return "", rejectf("%q is a reserved name on Windows", p)
		}
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		return "", rejectf("file path is empty")
	}
	if ext := strings.ToLower(path.Ext(parts[len(parts)-1])); !allowedWriteExts[ext] {
		return "", rejectf("file type %q is not allowed (allowed: %s)", ext, strings.Join(slices.Sorted(maps.Keys(allowedWriteExts)), " "))
	}
	return strings.Join(parts, "/"), nil
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// resolveDir returns an absolute, symlink-free form of dir. For a folder that
// doesn't exist yet (an unmounted drive, say) the nearest existing ancestor is
// resolved and the rest appended, so it still compares equal to resolved
// paths (/var vs /private/var on macOS).
func resolveDir(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = filepath.Clean(dir)
	}
	rest := ""
	for p := abs; ; p = filepath.Dir(p) {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(real, rest)
		}
		if filepath.Dir(p) == p {
			return abs
		}
		rest = filepath.Join(filepath.Base(p), rest)
	}
}

// caseInsensitiveFS reports whether path comparisons should fold case. APFS and
// NTFS are case-insensitive by default.
func caseInsensitiveFS() bool {
	return runtime.GOOS == "darwin" || runtime.GOOS == "windows"
}

// pathWithin reports whether child is parent or inside it.
func pathWithin(child, parent string) bool {
	if caseInsensitiveFS() {
		child, parent = strings.ToLower(child), strings.ToLower(parent)
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

type protectedDir struct {
	path string
	// allowInside permits write folders inside path while still refusing path
	// itself and its ancestors; used for the home folder.
	allowInside bool
	// except lists subfolders of path that may still be picked, such as iCloud
	// Drive, which lives under ~/Library.
	except []string
}

// protectedDirs lists folders a write folder may not be, contain, or (unless
// allowInside) sit inside: the Listener's own config and log folders, and the
// places where a dropped file is picked up and run automatically. The list
// can't be complete, which is why the extension allowlist exists as well.
var protectedDirs = sync.OnceValue(func() []protectedDir {
	var dirs []protectedDir
	add := func(p string, allowInside bool, except ...string) {
		if p == "" {
			return
		}
		resolved := make([]string, len(except))
		for i, e := range except {
			resolved[i] = resolveDir(e)
		}
		dirs = append(dirs, protectedDir{path: resolveDir(p), allowInside: allowInside, except: resolved})
	}
	if configPath != "" {
		add(filepath.Dir(configPath), false)
	}
	if logPath, err := appLogPath(); err == nil {
		add(filepath.Dir(logPath), false)
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		add(home, true)
	}
	switch runtime.GOOS {
	case "darwin":
		if home != "" {
			lib := filepath.Join(home, "Library")
			add(lib, false, filepath.Join(lib, "Mobile Documents"), filepath.Join(lib, "CloudStorage"))
		}
		for _, p := range []string{"/Library", "/System", "/Applications", "/usr", "/bin", "/sbin", "/etc", "/private/etc", "/private/var/db"} {
			add(p, false)
		}
	case "windows":
		for _, env := range []string{"APPDATA", "LOCALAPPDATA", "ProgramData", "SystemRoot", "ProgramFiles", "ProgramFiles(x86)"} {
			add(os.Getenv(env), false)
		}
	default:
		for _, p := range []string{"/etc", "/usr", "/bin", "/sbin", "/boot", "/var/lib"} {
			add(p, false)
		}
	}
	if runtime.GOOS != "windows" && home != "" {
		for _, p := range []string{".ssh", ".config", ".local", ".gnupg"} {
			add(filepath.Join(home, p), false)
		}
	}
	return dirs
})

// validateWriteRoot refuses folders where an appended file could be executed,
// could tamper with the Listener itself, or that would cover the whole disk or
// home folder. It returns the resolved folder on success.
func validateWriteRoot(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", errors.New("no folder chosen")
	}
	resolved := resolveDir(dir)
	if isDriveRoot(resolved) {
		return "", errors.New("For security, Listener can't write to a whole drive. Choose a folder on it instead.")
	}
	for _, p := range protectedDirs() {
		if pathWithin(p.path, resolved) {
			if p.allowInside {
				return "", errors.New("For security, Listener can't write to your whole home folder. Choose a folder inside it, such as Documents.")
			}
			if pathWithin(resolved, p.path) {
				return "", fmt.Errorf("For security, Listener can't write to %s. Choose a different folder, such as one in Documents.", displayPath(p.path))
			}
			return "", fmt.Errorf("For security, Listener can't use this folder because it contains %s. Choose a folder inside it instead.", displayPath(p.path))
		}
		if p.allowInside || !pathWithin(resolved, p.path) {
			continue
		}
		excepted := false
		for _, e := range p.except {
			if pathWithin(resolved, e) {
				excepted = true
				break
			}
		}
		if !excepted {
			return "", fmt.Errorf("For security, Listener can't write inside %s. Choose a different folder, such as one in Documents.", displayPath(p.path))
		}
	}
	return resolved, nil
}

// isDriveRoot reports whether dir is the top of a drive. On macOS other drives
// mount at /Volumes/<name>, so those (and /Volumes itself) count as well as "/".
func isDriveRoot(dir string) bool {
	if filepath.Dir(dir) == dir {
		return true
	}
	if runtime.GOOS == "darwin" {
		return strings.EqualFold(dir, "/Volumes") || strings.EqualFold(filepath.Dir(dir), "/Volumes")
	}
	return false
}

// displayPath shortens paths under the home folder to "~/...". Windows users
// don't know "~", so paths stay as they are there.
func displayPath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || runtime.GOOS == "windows" {
		return p
	}
	home = resolveDir(home)
	if rel, err := filepath.Rel(home, p); err == nil && pathWithin(p, home) {
		return filepath.Join("~", rel)
	}
	return p
}

// fileWriteRootsMessage lists the folder names and file types clients may use
// with fileWrite. It never includes the folders' real paths.
func fileWriteRootsMessage(msgType string) map[string]any {
	configMu.RLock()
	names := slices.Sorted(maps.Keys(config.FileWriteRoots))
	configMu.RUnlock()
	return map[string]any{
		"type":       msgType,
		"roots":      names,
		"extensions": slices.Sorted(maps.Keys(allowedWriteExts)),
	}
}

// broadcastFileWriteRoots tells authenticated clients the current folder
// names so pickers stay current. Nothing is sent while fileWrite is disabled.
func broadcastFileWriteRoots() {
	configMu.RLock()
	enabled := config.AllowedCommands["fileWrite"]
	configMu.RUnlock()
	if !enabled {
		return
	}
	msg := fileWriteRootsMessage("fileWriteRootsChanged")
	clientsMu.Lock()
	list := make([]*Client, 0, len(clients))
	for c := range clients {
		if c.authenticated.Load() {
			list = append(list, c)
		}
	}
	clientsMu.Unlock()
	// Off the GUI goroutine: a slow client mustn't stall the settings window.
	go func() {
		for _, c := range list {
			_ = c.writeJSON(msg)
		}
	}()
}

func newlineFor(mode string) (string, error) {
	switch strings.ToLower(mode) {
	case "", "lf":
		return "\n", nil
	case "crlf":
		return "\r\n", nil
	case "none":
		return "", nil
	}
	return "", rejectf("unknown newline %q (use lf, crlf or none)", mode)
}

// writeToFile appends msg plus a line ending to rel inside the write folder
// named rootName, creating the file and its parent folders as needed. It
// returns the folder name used (rootName may be empty when only one folder
// exists), the sanitized path and the number of bytes written. Clients only
// ever name a folder; the real path never leaves the server.
func writeToFile(rootName, rel, msg, newline string) (string, string, int, error) {
	configMu.RLock()
	names := slices.Sorted(maps.Keys(config.FileWriteRoots))
	if rootName == "" && len(names) == 1 {
		rootName = names[0]
	}
	dir, ok := config.FileWriteRoots[rootName]
	configMu.RUnlock()

	switch {
	case len(names) == 0:
		return rootName, "", 0, rejectf("no allowed write file paths are configured on this Listener")
	case rootName == "":
		return rootName, "", 0, rejectf("folder is required (available: %s)", strings.Join(names, ", "))
	case !ok:
		return rootName, "", 0, rejectf("folder %q is not configured on this Listener (available: %s)", rootName, strings.Join(names, ", "))
	}
	clean, n, err := writeInRoot(rootName, dir, rel, msg, newline)
	return rootName, clean, n, err
}

func writeInRoot(rootName, dir, rel, msg, newline string) (string, int, error) {
	nl, err := newlineFor(newline)
	if err != nil {
		return "", 0, err
	}
	data := msg + nl
	if len(data) > maxWriteBytes {
		return "", 0, rejectf("text is too long (%d bytes, limit %d)", len(data), maxWriteBytes)
	}
	clean, err := sanitizeWritePath(rel)
	if err != nil {
		return "", 0, err
	}
	// Re-checked on every write: the folder may have been replaced by a symlink
	// since it was added. The resolved path is what gets opened, so a symlink
	// swapped in after this check isn't followed.
	resolved, err := validateWriteRoot(dir)
	if err != nil {
		return clean, 0, rejectf("folder %q is no longer allowed. Check Allowed Write File Paths on the Listener", rootName)
	}

	fileWriteMu.Lock()
	defer fileWriteMu.Unlock()

	root, err := os.OpenRoot(resolved)
	if err != nil {
		return clean, 0, fmt.Errorf("folder %q is not available: %w", rootName, describeFileErr(err))
	}
	defer root.Close()
	if parent := path.Dir(clean); parent != "." {
		if err := root.MkdirAll(filepath.FromSlash(parent), 0755); err != nil {
			return clean, 0, describeFileErr(err)
		}
	}
	f, err := root.OpenFile(filepath.FromSlash(clean), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0644)
	if err != nil {
		return clean, 0, describeFileErr(err)
	}
	st, err := f.Stat()
	if err == nil && !st.Mode().IsRegular() {
		err = rejectf("%s is not a regular file", clean)
	} else if err == nil && st.Size()+int64(len(data)) > maxWriteFileSize {
		err = rejectf("%s has reached the %d MB size limit", clean, maxWriteFileSize/(1024*1024))
	}
	if err == nil {
		_, err = f.WriteString(data)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return clean, 0, describeFileErr(err)
	}
	return clean, len(data), nil
}

// errPathEscapes captures os.Root's unexported escape error once, by asking a
// root for its parent, so it can be matched by value rather than by message.
var errPathEscapes = sync.OnceValue(func() error {
	root, err := os.OpenRoot(os.TempDir())
	if err != nil {
		return nil
	}
	defer root.Close()
	_, err = root.Stat("..")
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return nil
})

// describeFileErr adds a hint for the failures users can fix themselves. The
// underlying error's path is dropped so the client never learns where the
// folder really lives.
func describeFileErr(err error) error {
	var rej *writeRejection
	if errors.As(err, &rej) {
		return err
	}
	if escapes := errPathEscapes(); escapes != nil && errors.Is(err, escapes) {
		return rejectf("file path leads outside the folder")
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	switch {
	case errFileInUse != nil && errors.Is(err, errFileInUse):
		return errors.New("file is in use by another program (close it in Excel or similar)")
	case errors.Is(err, fs.ErrPermission) && runtime.GOOS == "darwin":
		return errors.New("permission denied; check System Settings → Privacy & Security → Files and Folders for Bitfocus Listener")
	case errors.Is(err, fs.ErrPermission):
		return errors.New("permission denied")
	case errors.Is(err, fs.ErrNotExist):
		return errors.New("folder does not exist")
	}
	return err
}
