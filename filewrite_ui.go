package main

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// fileFoldersStatus summarizes the write folders for the Security section.
func fileFoldersStatus() string {
	configMu.RLock()
	n := len(config.FileWriteRoots)
	configMu.RUnlock()
	switch n {
	case 0:
		return "None"
	case 1:
		return "1 folder"
	}
	return fmt.Sprintf("%d folders", n)
}

// defaultPickerLocation opens the folder picker in Documents, where most
// people keep show files, falling back to the home folder.
func defaultPickerLocation() fyne.ListableURI {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	for _, dir := range []string{filepath.Join(home, "Documents"), home} {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			continue
		}
		if l, err := storage.ListerForURI(storage.NewFileURI(dir)); err == nil {
			return l
		}
	}
	return nil
}

// pathLabel shows a path shortened in the middle, keeping the start and the
// folder's own name visible: "~/Documents/…/Show Logs". Fyne labels can only
// cut text off at the end, which hides the part that matters.
type pathLabel struct {
	widget.Label
	full string
}

func newPathLabel(path string) *pathLabel {
	l := &pathLabel{full: path}
	l.Text = path
	l.ExtendBaseWidget(l)
	return l
}

// MinSize lets the label shrink below its text width; Resize shortens the text to fit.
func (l *pathLabel) MinSize() fyne.Size {
	return fyne.NewSize(measureLabelText("…")+2*theme.InnerPadding(), l.Label.MinSize().Height)
}

func (l *pathLabel) Resize(size fyne.Size) {
	if text := fitPath(l.full, size.Width-2*theme.InnerPadding()); text != l.Text {
		l.Text = text
		l.Refresh()
	}
	l.Label.Resize(size)
}

func measureLabelText(s string) float32 {
	return fyne.MeasureText(s, theme.TextSize(), fyne.TextStyle{}).Width
}

// fitPath drops components from the middle of path, replacing them with "…",
// until it fits width. As a last resort the folder name itself is cut from
// the start.
func fitPath(path string, width float32) string {
	if measureLabelText(path) <= width {
		return path
	}
	sep := string(filepath.Separator)
	// Leading separators ("/" or a UNC "\\") stay attached to the first part.
	trimmed := strings.TrimLeft(path, sep)
	prefix := path[:len(path)-len(trimmed)]
	parts := strings.Split(trimmed, sep)
	if len(parts) > 2 {
		head := prefix + parts[0]
		for keep := len(parts) - 2; keep >= 1; keep-- {
			candidate := head + sep + "…" + sep + strings.Join(parts[len(parts)-keep:], sep)
			if measureLabelText(candidate) <= width {
				return candidate
			}
		}
	}
	last := []rune(parts[len(parts)-1])
	for i := range last {
		if candidate := "…" + string(last[i:]); measureLabelText(candidate) <= width {
			return candidate
		}
	}
	return "…"
}

// showNotice is a one-button sheet for problems the user can fix.
func showNotice(parent fyne.Window, title, message string) {
	d := newSheet(parent, title, message, nil,
		sheetButton{label: "OK", importance: widget.HighImportance})
	d.Resize(fyne.NewSize(400, 0))
	d.Show()
}

// suggestRootName derives a folder name clients can type from the folder's
// base name, made unique against taken.
func suggestRootName(dir string, taken func(string) bool) string {
	base := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		}
		return '-'
	}, filepath.Base(dir))
	base = strings.Trim(base, "-")
	// Leave room for a "-N" suffix.
	if len(base) > maxRootNameLen-3 {
		base = strings.TrimRight(base[:maxRootNameLen-3], "-")
	}
	if base == "" {
		base = "folder"
	}
	name := base
	for i := 2; taken(name); i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return name
}

// nameColumnWidth fits a maximum-length name, so the entry never needs to
// scroll; unusually wide glyphs are clipped by the entry instead.
func nameColumnWidth() float32 {
	text := fyne.MeasureText(strings.Repeat("m", maxRootNameLen), theme.TextSize(), fyne.TextStyle{})
	return text.Width + 2*theme.InnerPadding()
}

func showFileFoldersDialog(parent fyne.Window, onSaved func()) {
	type folderRow struct {
		name *escapeEntry
		path string
	}
	var rows []*folderRow

	configMu.RLock()
	before := make(map[string]string, len(config.FileWriteRoots))
	for name, dir := range config.FileWriteRoots {
		before[name] = dir
	}
	configMu.RUnlock()

	list := container.NewVBox()
	errLabel := widget.NewLabel("")
	errLabel.Importance = widget.DangerImportance
	errLabel.Wrapping = fyne.TextWrapWord
	errLabel.Hide()

	validate := func() error {
		seen := make(map[string]bool, len(rows))
		for _, r := range rows {
			name := strings.TrimSpace(r.name.Text)
			if !validRootName.MatchString(name) {
				if name == "" {
					return errors.New("Every folder needs a name.")
				}
				return fmt.Errorf("“%s” can only use letters, numbers, - and _.", name)
			}
			if seen[strings.ToLower(name)] {
				return fmt.Errorf("The name “%s” is used more than once.", name)
			}
			seen[strings.ToLower(name)] = true
		}
		return nil
	}
	revalidate := func() {
		if err := validate(); err != nil {
			errLabel.SetText(err.Error())
			errLabel.Show()
		} else {
			errLabel.Hide()
		}
	}

	var rebuild func()
	rebuild = func() {
		list.Objects = nil
		if len(rows) == 0 {
			empty := widget.NewLabel("No folders yet. Add one so buttons can save to files.")
			empty.Wrapping = fyne.TextWrapWord
			list.Add(empty)
		} else {
			nameHead := newSubtleText("Name")
			subtleStyle(nameHead).TextStyle = fyne.TextStyle{Bold: true}
			folderHead := newSubtleText("Folder")
			subtleStyle(folderHead).TextStyle = fyne.TextStyle{Bold: true}
			headBox := container.NewGridWrap(fyne.NewSize(nameColumnWidth(), nameHead.MinSize().Height), nameHead)
			list.Add(container.NewBorder(nil, nil, headBox, nil, folderHead))
			list.Add(widget.NewSeparator())
		}
		for i, r := range rows {
			if i > 0 {
				list.Add(widget.NewSeparator())
			}
			pathLabel := newPathLabel(displayPath(r.path))
			remove := newIconButton(theme.DeleteIcon(), func() {
				for j, other := range rows {
					if other == r {
						rows = append(rows[:j], rows[j+1:]...)
						break
					}
				}
				rebuild()
			})
			nameBox := container.NewGridWrap(fyne.NewSize(nameColumnWidth(), r.name.MinSize().Height), r.name)
			list.Add(container.NewBorder(nil, nil, nameBox, remove, pathLabel))
		}
		list.Refresh()
		revalidate()
	}

	addRow := func(name, dir string) {
		e := &escapeEntry{}
		e.ExtendBaseWidget(e)
		e.SetPlaceHolder("name")
		e.SetText(name)
		e.OnChanged = func(text string) {
			if r := []rune(text); len(r) > maxRootNameLen {
				e.SetText(string(r[:maxRootNameLen]))
				return
			}
			revalidate()
		}
		rows = append(rows, &folderRow{name: e, path: dir})
	}
	for _, name := range slices.Sorted(maps.Keys(before)) {
		addRow(name, before[name])
	}

	addPicked := func(picked string) {
		dir, err := validateWriteRoot(picked)
		if err != nil {
			showNotice(parent, "Folder Not Allowed", err.Error())
			return
		}
		for _, r := range rows {
			if resolveDir(r.path) == dir {
				showNotice(parent, "Folder Already Added", fmt.Sprintf("This folder is already in the list as “%s”.", r.name.Text))
				return
			}
		}
		name := suggestRootName(dir, func(n string) bool {
			for _, r := range rows {
				if strings.EqualFold(strings.TrimSpace(r.name.Text), n) {
					return true
				}
			}
			return false
		})
		addRow(name, dir)
		rebuild()
	}

	addFolder := newKeyButton("Add Folder…", func() {
		fd := dialog.NewFolderOpen(func(uri fyne.ListableURI, err error) {
			if err != nil {
				showNotice(parent, "Something Went Wrong", err.Error())
				return
			}
			if uri != nil {
				addPicked(uri.Path())
			}
		}, parent)
		fd.SetTitleText("Choose a Folder")
		fd.SetConfirmText("Use Folder")
		fd.SetView(dialog.ListView)
		if start := defaultPickerLocation(); start != nil {
			fd.SetLocation(start)
		}
		fd.Show()
	})
	rebuild()

	// Dropping folders from Finder/Explorer anywhere on the window adds them,
	// which also reaches external drives the picker's sidebar doesn't list.
	parent.SetOnDropped(func(_ fyne.Position, uris []fyne.URI) {
		for _, u := range uris {
			if u.Scheme() != "file" {
				continue
			}
			if st, err := os.Stat(u.Path()); err != nil || !st.IsDir() {
				showNotice(parent, "Not a Folder", fmt.Sprintf("“%s” is a file. Drop a folder instead.", filepath.Base(u.Path())))
				continue
			}
			addPicked(u.Path())
		}
	})

	// save reports whether the folders were stored; on failure the sheet
	// stays open with the user's edits.
	save := func() bool {
		revalidate()
		if validate() != nil {
			return false
		}
		after := make(map[string]string, len(rows))
		for _, r := range rows {
			after[strings.TrimSpace(r.name.Text)] = r.path
		}
		var changes []string
		for name, dir := range after {
			if old, ok := before[name]; !ok || old != dir {
				changes = append(changes, fmt.Sprintf("%s=%s", name, dir))
			}
		}
		for name := range before {
			if _, ok := after[name]; !ok {
				changes = append(changes, name+" removed")
			}
		}
		// Only switch to the new folders once they are on disk, so a failed save
		// doesn't leave clients writing somewhere that disappears on restart.
		configMu.Lock()
		next := config
		next.FileWriteRoots = after
		err := saveConfigLocked(&next)
		if err == nil {
			config.FileWriteRoots = after
		}
		configMu.Unlock()
		if err != nil {
			showNotice(parent, "Changes Not Saved", err.Error())
			return false
		}
		if len(changes) > 0 {
			auditLog("gui", "FILE_FOLDERS_CHANGED", strings.Join(changes, ", "))
			broadcastFileWriteRoots()
		}
		if onSaved != nil {
			onSaved()
		}
		return true
	}

	content := container.NewBorder(nil,
		container.NewVBox(errLabel, container.NewHBox(addFolder)),
		nil, nil,
		container.NewVScroll(newCard(list)),
	)
	d := newSheet(parent, "Allowed Write File Paths",
		"Select the folders where Listener can create and write to files. Clients will use the name to "+
			"choose a folder. Add one below or drag a folder onto this window."+
			"\n\nAllowed file types: plain-text data files (.txt, .csv, .json, .srt, …)",
		content,
		sheetButton{label: "Cancel"},
		sheetButton{label: "Save", importance: widget.HighImportance, canClose: save},
	)
	d.SetOnClosed(func() { parent.SetOnDropped(nil) })
	d.Resize(fyne.NewSize(560, 600))
	d.Show()
}
