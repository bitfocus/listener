package main

import (
	"image/color"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// Accent matches the amber ear in icon.png. The light variant is darker so
// checkmarks and focus rings keep 3:1 contrast on a white background.
var (
	accentDark  = color.NRGBA{R: 0xF0, G: 0xBE, B: 0x3A, A: 0xFF}
	accentLight = color.NRGBA{R: 0xB4, G: 0x7F, B: 0x00, A: 0xFF}
)

// Replacements for Fyne defaults that miss WCAG AA against our card surfaces
// (4.5:1 for text, 3:1 for control outlines and status dots).
var (
	darkContrast = map[fyne.ThemeColorName]color.Color{
		theme.ColorNameInputBorder: color.NRGBA{R: 0x6E, G: 0x6E, B: 0x6F, A: 0xFF},
		theme.ColorNameError:       color.NRGBA{R: 0xFF, G: 0x50, B: 0x40, A: 0xFF},
	}
	lightContrast = map[fyne.ThemeColorName]color.Color{
		theme.ColorNameInputBorder: color.NRGBA{R: 0x8A, G: 0x8A, B: 0x8A, A: 0xFF},
		theme.ColorNamePlaceHolder: color.NRGBA{R: 0x6E, G: 0x6E, B: 0x6E, A: 0xFF},
		theme.ColorNameError:       color.NRGBA{R: 0xD6, G: 0x26, B: 0x26, A: 0xFF},
		theme.ColorNameSuccess:     color.NRGBA{R: 0x1E, G: 0xA0, B: 0x3A, A: 0xFF},
		theme.ColorNameWarning:     color.NRGBA{R: 0xD2, G: 0x73, B: 0x00, A: 0xFF},
	}
)

// listenerTheme is the default theme with our accent, contrast fixes and
// neutral dark surfaces. The replaced colours are computed once per variant.
type listenerTheme struct {
	fyne.Theme
	colors map[fyne.ThemeVariant]map[fyne.ThemeColorName]color.Color
}

func newListenerTheme() listenerTheme {
	base := theme.DefaultTheme()
	return listenerTheme{Theme: base, colors: map[fyne.ThemeVariant]map[fyne.ThemeColorName]color.Color{
		theme.VariantDark:  themeColors(base, theme.VariantDark),
		theme.VariantLight: themeColors(base, theme.VariantLight),
	}}
}

func themeColors(base fyne.Theme, v fyne.ThemeVariant) map[fyne.ThemeColorName]color.Color {
	accent, contrast := accentLight, lightContrast
	if v == theme.VariantDark {
		accent, contrast = accentDark, darkContrast
	}
	colors := map[fyne.ThemeColorName]color.Color{
		theme.ColorNamePrimary:             accent,
		theme.ColorNameForegroundOnPrimary: color.NRGBA{R: 0x1C, G: 0x1B, B: 0x1A, A: 0xFF},
		theme.ColorNameFocus:               withAlpha(accent, 0x7F),
		theme.ColorNameSelection:           withAlpha(accent, 0x3F),
	}
	for n, c := range contrast {
		colors[n] = c
	}
	// Fyne's dark surfaces are tinted navy; derive them from the neutral window
	// background instead so popups and controls match the main screen.
	if v == theme.VariantDark {
		bg := base.Color(theme.ColorNameBackground, v)
		fg := base.Color(theme.ColorNameForeground, v)
		colors[theme.ColorNameOverlayBackground] = mix(bg, fg, 0.05)
		colors[theme.ColorNameInputBackground] = mix(bg, fg, 0.06)
		colors[theme.ColorNameScrollBarBackground] = mix(bg, fg, 0.06)
		colors[theme.ColorNameButton] = mix(bg, fg, 0.11)
		colors[theme.ColorNameDisabledButton] = mix(bg, fg, 0.11)
		colors[theme.ColorNameMenuBackground] = mix(bg, fg, 0.11)
	}
	return colors
}

func (t listenerTheme) Color(n fyne.ThemeColorName, v fyne.ThemeVariant) color.Color {
	if c, ok := t.colors[v][n]; ok {
		return c
	}
	return t.Theme.Color(n, v)
}

func (t listenerTheme) Size(n fyne.ThemeSizeName) float32 {
	switch n {
	case theme.SizeNameInputRadius:
		return 6
	case theme.SizeNameCaptionText:
		// Fyne's 11pt caption is used for headings, counts and timestamps here.
		return 12
	}
	return t.Theme.Size(n)
}

// mix blends a toward b by amount (0..1), returning an opaque colour.
func mix(a, b color.Color, amount float64) color.Color {
	ar, ag, ab, _ := a.RGBA()
	br, bg, bb, _ := b.RGBA()
	ch := func(x, y uint32) uint8 {
		return uint8((float64(x)*(1-amount) + float64(y)*amount) / 0x101)
	}
	return color.NRGBA{R: ch(ar, br), G: ch(ag, bg), B: ch(ab, bb), A: 0xFF}
}

func withAlpha(base color.Color, alpha uint8) color.Color {
	r, g, b, _ := base.RGBA()
	return color.NRGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: alpha}
}

type statusLevel int

const (
	statusIdle statusLevel = iota
	statusOK
	statusError
)

func (l statusLevel) color() color.Color {
	switch l {
	case statusOK:
		return theme.Color(theme.ColorNameSuccess)
	case statusError:
		return theme.Color(theme.ColorNameError)
	}
	return theme.Color(theme.ColorNamePlaceHolder)
}

// statusIndicator is a coloured dot followed by text. The text stays in the
// foreground colour except for errors, so normal idle states read calmly.
type statusIndicator struct {
	widget.BaseWidget
	dot   *canvas.Rectangle
	label *widget.Label
	level statusLevel
}

func newStatusIndicator(level statusLevel, text string) *statusIndicator {
	s := &statusIndicator{
		dot:   canvas.NewRectangle(color.Transparent),
		label: widget.NewLabel(text),
		level: level,
	}
	s.dot.SetMinSize(fyne.NewSize(8, 8))
	s.dot.CornerRadius = 4
	s.ExtendBaseWidget(s)
	return s
}

func (s *statusIndicator) CreateRenderer() fyne.WidgetRenderer {
	s.applyLevel()
	return widget.NewSimpleRenderer(container.New(statusLayout{}, s.dot, s.label))
}

// statusLayout places the dot where label text would start, so it lines up
// with plain labels in the same column, and keeps a tight gap to the text.
// Label draws its text from the top of its box, so the label is kept at its
// min height and centred; otherwise taller rows leave the text above the dot.
type statusLayout struct{}

const statusDotGap = 6

// The label starts at dot width + gap; its own inner padding then puts the
// text just right of the dot, which itself sits at the label inset.
func (statusLayout) MinSize(objs []fyne.CanvasObject) fyne.Size {
	dot, label := objs[0].MinSize(), objs[1].MinSize()
	return fyne.NewSize(dot.Width+statusDotGap+label.Width, fyne.Max(dot.Height, label.Height))
}

func (statusLayout) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	dot, label := objs[0], objs[1]
	inset := theme.InnerPadding()
	dotSize := dot.MinSize()
	labelH := label.MinSize().Height
	labelY := (size.Height - labelH) / 2
	labelX := dotSize.Width + statusDotGap
	label.Resize(fyne.NewSize(size.Width-labelX, labelH))
	label.Move(fyne.NewPos(labelX, labelY))
	dot.Resize(dotSize)
	dot.Move(fyne.NewPos(inset, labelY+(labelH-dotSize.Height)/2))
}

func (s *statusIndicator) Set(level statusLevel, text string) {
	if s.level == level && s.label.Text == text {
		return
	}
	s.level = level
	s.label.Text = text
	s.Refresh()
}

// Refresh is also called by Fyne on light/dark changes, which re-resolves the dot colour.
func (s *statusIndicator) Refresh() {
	s.applyLevel()
	s.BaseWidget.Refresh()
}

func (s *statusIndicator) applyLevel() {
	s.dot.FillColor = s.level.color()
	if s.level == statusError {
		s.label.Importance = widget.DangerImportance
	} else {
		s.label.Importance = widget.MediumImportance
	}
}

// card is a rounded, subtly tinted panel grouping related rows.
type card struct {
	widget.BaseWidget
	bg      *canvas.Rectangle
	content fyne.CanvasObject
}

// newCard stacks objs vertically; a single object is used as-is so that
// expanding content such as a List fills the card.
func newCard(objs ...fyne.CanvasObject) *card {
	content := objs[0]
	if len(objs) > 1 {
		content = container.NewVBox(objs...)
	}
	c := &card{bg: canvas.NewRectangle(color.Transparent), content: content}
	c.bg.CornerRadius = 10
	c.bg.StrokeWidth = 1
	c.ExtendBaseWidget(c)
	return c
}

func (c *card) CreateRenderer() fyne.WidgetRenderer {
	c.applyColors()
	return widget.NewSimpleRenderer(container.NewStack(c.bg, container.NewPadded(c.content)))
}

func (c *card) Refresh() {
	c.applyColors()
	c.bg.Refresh()
	c.BaseWidget.Refresh()
}

func (c *card) applyColors() {
	fg := theme.Color(theme.ColorNameForeground)
	c.bg.FillColor = withAlpha(fg, 0x0C)
	c.bg.StrokeColor = withAlpha(fg, 0x1A)
}

// errorBanner is a solid error-coloured card with light text, for problems
// that stop the app from working.
type errorBanner struct {
	widget.BaseWidget
	bg     *canvas.Rectangle
	title  *widget.Label
	detail *widget.Label
}

func newErrorBanner() *errorBanner {
	b := &errorBanner{
		bg:     canvas.NewRectangle(color.Transparent),
		title:  widget.NewLabel(""),
		detail: widget.NewLabel(""),
	}
	b.bg.CornerRadius = 10
	b.title.TextStyle = fyne.TextStyle{Bold: true}
	b.title.Wrapping = fyne.TextWrapWord
	b.detail.Wrapping = fyne.TextWrapWord
	b.ExtendBaseWidget(b)
	return b
}

func (b *errorBanner) Set(title, detail string) {
	b.title.SetText(title)
	b.detail.SetText(detail)
}

func (b *errorBanner) CreateRenderer() fyne.WidgetRenderer {
	b.bg.FillColor = theme.Color(theme.ColorNameError)
	icon := widget.NewIcon(theme.NewColoredResource(theme.ErrorIcon(), theme.ColorNameForeground))
	p := theme.Padding()
	iconBox := container.New(layout.NewCustomPaddedLayout(0, 0, p, p), container.NewCenter(icon))
	// Each label pads itself, so pull the detail up under the headline.
	lines := container.New(layout.NewCustomPaddedVBoxLayout(-1.5*theme.InnerPadding()), b.title, b.detail)
	text := container.New(layout.NewCustomPaddedLayout(0, 0, 0, p), lines)
	// Light text reads on the red fill in both light and dark mode.
	body := container.NewThemeOverride(container.NewBorder(nil, nil, iconBox, nil, text),
		lightForegroundTheme{fyne.CurrentApp().Settings().Theme()})
	return widget.NewSimpleRenderer(container.NewStack(b.bg, container.NewPadded(body)))
}

func (b *errorBanner) Refresh() {
	b.bg.FillColor = theme.Color(theme.ColorNameError)
	b.bg.Refresh()
	b.BaseWidget.Refresh()
}

type lightForegroundTheme struct{ fyne.Theme }

func (t lightForegroundTheme) Color(n fyne.ThemeColorName, v fyne.ThemeVariant) color.Color {
	if n == theme.ColorNameForeground {
		return color.White
	}
	return t.Theme.Color(n, v)
}

func newSection(title string, objs ...fyne.CanvasObject) fyne.CanvasObject {
	return newSectionWithAction(title, nil, objs...)
}

// newSectionWithAction is newSection with a control (such as an "Allow all"
// checkbox) on the right of the heading.
func newSectionWithAction(title string, action fyne.CanvasObject, objs ...fyne.CanvasObject) fyne.CanvasObject {
	heading := newSubtleText(title)
	subtleStyle(heading).TextStyle = fyne.TextStyle{Bold: true}
	var top fyne.CanvasObject = heading
	if action != nil {
		top = container.NewBorder(nil, nil, nil, action, container.NewVBox(layout.NewSpacer(), heading, layout.NewSpacer()))
	}
	return container.NewVBox(top, newCard(objs...))
}

// tapRow is a full-width clickable row with a hover highlight, used for
// settings that open a dialog or trigger an action. It takes keyboard focus
// so Tab reaches it, and Space or Return activates it.
type tapRow struct {
	widget.BaseWidget
	bg      *canvas.Rectangle
	content fyne.CanvasObject
	icon    *widget.Icon
	onTap   func()
	hovered bool
	focused bool
	// ring is whether the focus ring shows: only for keyboard focus, not the
	// focus Fyne gives a Focusable widget on mouse-down.
	ring bool
}

var (
	_ fyne.Tappable      = (*tapRow)(nil)
	_ fyne.Focusable     = (*tapRow)(nil)
	_ desktop.Hoverable  = (*tapRow)(nil)
	_ desktop.Cursorable = (*tapRow)(nil)
)

// newTapRow builds a row with title on the left and trailing content plus an
// icon (a chevron when icon is nil) on the right.
func newTapRow(title string, trailing fyne.CanvasObject, icon fyne.Resource, onTap func()) *tapRow {
	if icon == nil {
		icon = theme.NavigateNextIcon()
	}
	iconWidget := widget.NewIcon(icon)
	right := container.NewHBox(trailingInset(iconWidget))
	if trailing != nil {
		right.Objects = append([]fyne.CanvasObject{trailing}, right.Objects...)
	}
	r := &tapRow{
		bg:      canvas.NewRectangle(color.Transparent),
		content: container.NewBorder(nil, nil, widget.NewLabel(title), right),
		icon:    iconWidget,
		onTap:   onTap,
	}
	r.bg.CornerRadius = 6
	r.bg.StrokeWidth = 2
	r.ExtendBaseWidget(r)
	return r
}

func (r *tapRow) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(container.NewStack(r.bg, r.content))
}

// Tapped handles a mouse click. It drops the hover state (a sheet opened
// over the row would swallow MouseOut) and releases the focus that the click
// gave the row, so no ring lingers and no sheet restores focus here.
func (r *tapRow) Tapped(*fyne.PointEvent) {
	r.hovered = false
	if r.focused {
		if c := fyne.CurrentApp().Driver().CanvasForObject(r); c != nil {
			c.Unfocus()
		}
	}
	r.updateBg()
	r.activate()
}

func (r *tapRow) activate() {
	if r.onTap != nil {
		r.onTap()
	}
}

func (r *tapRow) SetIcon(res fyne.Resource) { r.icon.SetResource(res) }

func (r *tapRow) Cursor() desktop.Cursor { return desktop.PointerCursor }

func (r *tapRow) FocusGained() {
	r.focused = true
	r.ring = !r.hovered
	r.updateBg()
}

func (r *tapRow) FocusLost() {
	r.focused = false
	r.ring = false
	r.updateBg()
}

func (r *tapRow) TypedRune(rn rune) {
	if rn == ' ' {
		r.activate()
	}
}

func (r *tapRow) TypedKey(ev *fyne.KeyEvent) {
	switch ev.Name {
	case fyne.KeyReturn, fyne.KeyEnter:
		r.activate()
	case fyne.KeyEscape:
		sheetKey(ev)
	}
}

func (r *tapRow) MouseIn(*desktop.MouseEvent) {
	r.hovered = true
	r.updateBg()
}

// MouseMoved restores the hover cleared by Tapped once the pointer moves again.
func (r *tapRow) MouseMoved(*desktop.MouseEvent) {
	if !r.hovered {
		r.hovered = true
		r.updateBg()
	}
}

func (r *tapRow) MouseOut() {
	r.hovered = false
	r.updateBg()
}

// updateBg redraws only the background; hover and focus change nothing else.
func (r *tapRow) updateBg() {
	if r.hovered {
		r.bg.FillColor = theme.Color(theme.ColorNameHover)
	} else {
		r.bg.FillColor = color.Transparent
	}
	if r.ring {
		r.bg.StrokeColor = theme.Color(theme.ColorNameFocus)
	} else {
		r.bg.StrokeColor = color.Transparent
	}
	r.bg.Refresh()
}

func (r *tapRow) Refresh() {
	r.updateBg()
	r.BaseWidget.Refresh()
}

// newSubtleText is caption-sized secondary text in the placeholder colour.
func newSubtleText(text string) *widget.RichText {
	return widget.NewRichText(&widget.TextSegment{
		Text: text,
		Style: widget.RichTextStyle{
			ColorName: theme.ColorNamePlaceHolder,
			SizeName:  theme.SizeNameCaptionText,
			Inline:    true,
		},
	})
}

// subtleStyle is the style of text made by newSubtleText, for callers that
// need monospace, bold or a different alignment.
func subtleStyle(rt *widget.RichText) *widget.RichTextStyle {
	return &rt.Segments[0].(*widget.TextSegment).Style
}

func setSubtleText(rt *widget.RichText, text string) {
	rt.Segments[0].(*widget.TextSegment).Text = text
	rt.Refresh()
}

// trailingInset pads o on the right so a trailing icon's edge mirrors the
// text inset of labels on the left.
func trailingInset(o fyne.CanvasObject) fyne.CanvasObject {
	return container.New(layout.NewCustomPaddedLayout(0, 0, 0, theme.Padding()), o)
}

const flashDuration = 1500 * time.Millisecond

// flash applies a temporary state now and resets it after flashDuration.
func flash(set, reset func()) {
	set()
	time.AfterFunc(flashDuration, func() { fyne.Do(reset) })
}

func newIconButton(icon fyne.Resource, onTapped func()) *keyButton {
	btn := newKeyButton("", onTapped)
	btn.Icon = icon
	btn.Importance = widget.LowImportance
	return btn
}

// newCopyButton copies value() to the clipboard and briefly shows a tick.
func newCopyButton(clip fyne.Clipboard, value func() string) *keyButton {
	var btn *keyButton
	btn = newIconButton(theme.ContentCopyIcon(), func() {
		clip.SetContent(value())
		flash(func() { btn.SetIcon(theme.ConfirmIcon()) }, func() { btn.SetIcon(theme.ContentCopyIcon()) })
	})
	return btn
}

// setChecked changes a checkbox without calling its OnChanged.
func setChecked(cb *widget.Check, on bool) {
	cb.Partial = false
	cb.Checked = on
	cb.Refresh()
}

type sheetButton struct {
	label      string
	importance widget.Importance
	onTap      func()
	// canClose, when set, runs first; returning false keeps the sheet open and
	// skips onTap, so a form with errors doesn't lose what the user typed.
	canClose func() bool
}

// sheet is a modal panel used instead of Fyne's dialogs, whose fixed layout
// reserves a title row and centres icon buttons at the bottom.
type sheet struct {
	pop         *widget.PopUp
	canvas      fyne.Canvas
	onClosed    func()
	primary     func()
	restoreKeys func()
	prevFocus   fyne.Focusable
}

// openSheets is the stack of visible sheets; the top one gets Escape/Return.
var openSheets []*sheet

// sheetKey sends Escape (cancel) and Return (primary action) to the topmost
// open sheet and reports whether it took the key. Fyne gives keys only to the
// focused widget while one has focus, so the widgets used in sheets
// (keyCheck, keyButton, tapRow, escapeEntry) forward these keys here; the
// canvas hook set in Show covers the case where nothing is focused.
func sheetKey(ev *fyne.KeyEvent) bool {
	if len(openSheets) == 0 {
		return false
	}
	s := openSheets[len(openSheets)-1]
	switch ev.Name {
	case fyne.KeyEscape:
		s.Hide()
		return true
	case fyne.KeyReturn, fyne.KeyEnter:
		if s.primary != nil {
			s.primary()
			return true
		}
	}
	return false
}

// sheetMargin keeps sheets clear of the window edges.
const sheetMargin = 16

// Show opens the sheet, routes Escape and Return to it (see sheetKey) and
// remembers the focused widget so Hide can return focus to it.
func (s *sheet) Show() {
	s.prevFocus = s.canvas.Focused()
	openSheets = append(openSheets, s)
	handle := func(ev *fyne.KeyEvent) { sheetKey(ev) }
	if dc, ok := s.canvas.(desktop.Canvas); ok {
		prev := dc.OnKeyDown()
		dc.SetOnKeyDown(handle)
		s.restoreKeys = func() { dc.SetOnKeyDown(prev) }
	} else {
		prev := s.canvas.OnTypedKey()
		s.canvas.SetOnTypedKey(handle)
		s.restoreKeys = func() { s.canvas.SetOnTypedKey(prev) }
	}
	s.canvas.Unfocus()
	s.pop.Show()
}

func (s *sheet) SetOnClosed(fn func()) { s.onClosed = fn }

// Resize clamps to the window, less a margin. A zero height fits the content:
// wrapped labels only report their real height once laid out at the final
// width, so lay out first, then measure.
func (s *sheet) Resize(size fyne.Size) {
	avail := s.canvas.Size()
	size.Width = fyne.Min(size.Width, avail.Width-2*sheetMargin)
	if size.Height == 0 {
		s.pop.Resize(fyne.NewSize(size.Width, s.pop.MinSize().Height))
		size.Height = s.pop.MinSize().Height
	}
	size.Height = fyne.Min(size.Height, avail.Height-2*sheetMargin)
	s.pop.Resize(size)
}

func (s *sheet) Hide() {
	if !s.pop.Visible() {
		return
	}
	for i, open := range openSheets {
		if open == s {
			openSheets = append(openSheets[:i], openSheets[i+1:]...)
			break
		}
	}
	s.restoreKeys()
	s.pop.Hide()
	if s.prevFocus != nil {
		s.canvas.Focus(s.prevFocus)
	}
	if s.onClosed != nil {
		s.onClosed()
	}
}

// newSheet builds a modal with a heading, optional description and content,
// and plain right-aligned buttons. Every button closes the sheet before
// running its action. Callers set the size and call Show.
func newSheet(parent fyne.Window, title, message string, content fyne.CanvasObject, buttons ...sheetButton) *sheet {
	heading := widget.NewLabel(title)
	heading.TextStyle = fyne.TextStyle{Bold: true}
	heading.SizeName = theme.SizeNameSubHeadingText
	top := container.NewVBox(heading)
	if message != "" {
		msg := widget.NewLabel(message)
		msg.Wrapping = fyne.TextWrapWord
		top.Add(msg)
	}

	s := &sheet{canvas: parent.Canvas()}
	row := container.NewHBox(layout.NewSpacer())
	for _, b := range buttons {
		btn := newKeyButton(b.label, func() {
			if b.canClose != nil && !b.canClose() {
				return
			}
			s.Hide()
			if b.onTap != nil {
				b.onTap()
			}
		})
		btn.Importance = b.importance
		row.Add(btn)
		// Return never confirms a destructive action, matching macOS.
		if b.importance == widget.HighImportance {
			s.primary = btn.OnTapped
		}
	}
	bottom := container.New(layout.NewCustomPaddedLayout(2*theme.Padding(), 0, 0, 0), row)

	if content == nil {
		content = layout.NewSpacer()
	}
	inset := 3 * theme.Padding()
	body := container.New(layout.NewCustomPaddedLayout(inset, inset, inset, inset),
		container.NewBorder(top, bottom, nil, nil, content))
	s.pop = widget.NewModalPopUp(body, parent.Canvas())
	return s
}

// escapeEntry is an Entry that lets its owner handle Escape first (to clear
// a search, say) and otherwise passes Escape to the open sheet. Return is
// left to the entry so typing never submits the sheet.
type escapeEntry struct {
	widget.Entry
	// OnEscape reports whether it handled the key.
	OnEscape func() bool
}

func (e *escapeEntry) TypedKey(ev *fyne.KeyEvent) {
	if ev.Name == fyne.KeyEscape {
		if (e.OnEscape != nil && e.OnEscape()) || sheetKey(ev) {
			return
		}
	}
	e.Entry.TypedKey(ev)
}

// keyCheck is a Check that passes Escape and Return to the open sheet.
type keyCheck struct{ widget.Check }

func newKeyCheck(label string, changed func(bool)) *keyCheck {
	c := &keyCheck{}
	c.Text = label
	c.OnChanged = changed
	c.ExtendBaseWidget(c)
	return c
}

func (c *keyCheck) TypedKey(ev *fyne.KeyEvent) {
	if !sheetKey(ev) {
		c.Check.TypedKey(ev)
	}
}

// keyButton is a Button that passes Escape and Return to the open sheet;
// Space still presses the button itself.
type keyButton struct{ widget.Button }

func newKeyButton(label string, tapped func()) *keyButton {
	b := &keyButton{}
	b.Text = label
	b.OnTapped = tapped
	b.ExtendBaseWidget(b)
	return b
}

func (b *keyButton) TypedKey(ev *fyne.KeyEvent) {
	if !sheetKey(ev) {
		b.Button.TypedKey(ev)
	}
}

// newSearchEntry is an Entry with a search icon inset from its right edge.
func newSearchEntry(placeholder string) *escapeEntry {
	e := &escapeEntry{}
	e.ExtendBaseWidget(e)
	e.SetPlaceHolder(placeholder)
	e.ActionItem = trailingInset(widget.NewIcon(theme.SearchIcon()))
	return e
}

// compactTheme shrinks text padding for dense lists such as the activity log,
// where Fyne's default insets around every line add up quickly.
type compactTheme struct{ fyne.Theme }

func (t compactTheme) Size(n fyne.ThemeSizeName) float32 {
	switch n {
	case theme.SizeNameInnerPadding:
		return 3
	case theme.SizeNameLineSpacing:
		return 2
	}
	return t.Theme.Size(n)
}

// readableDisabledTheme draws disabled controls in the secondary text colour.
// Locked key checkboxes are disabled but still convey which keys are allowed,
// and Fyne's disabled grey is too faint to read (about 1.4:1).
type readableDisabledTheme struct{ fyne.Theme }

func (t readableDisabledTheme) Color(n fyne.ThemeColorName, v fyne.ThemeVariant) color.Color {
	if n == theme.ColorNameDisabled {
		return t.Theme.Color(theme.ColorNamePlaceHolder, v)
	}
	return t.Theme.Color(n, v)
}

// digitEntry is an Entry that only accepts digits, up to maxLen of them.
// Pasted text is filtered to its digits. OnFocusLost lets callers repair an
// incomplete value when the user moves on.
type digitEntry struct {
	widget.Entry
	maxLen      int
	OnFocusLost func()
}

func newDigitEntry(maxLen int) *digitEntry {
	e := &digitEntry{maxLen: maxLen}
	e.ExtendBaseWidget(e)
	return e
}

func (e *digitEntry) TypedRune(r rune) {
	if r < '0' || r > '9' {
		return
	}
	// A selection is replaced by the typed digit, so it doesn't add length.
	if e.SelectedText() == "" && len(e.Text) >= e.maxLen {
		return
	}
	e.Entry.TypedRune(r)
}

func (e *digitEntry) TypedShortcut(s fyne.Shortcut) {
	paste, ok := s.(*fyne.ShortcutPaste)
	if !ok {
		e.Entry.TypedShortcut(s)
		return
	}
	for _, r := range paste.Clipboard.Content() {
		e.TypedRune(r)
	}
}

func (e *digitEntry) FocusLost() {
	e.Entry.FocusLost()
	if e.OnFocusLost != nil {
		e.OnFocusLost()
	}
}

// fillThenLayout sizes a single object to fill its container, then calls
// after. It lets callers react to resizes that fire no callback of their own.
type fillThenLayout struct{ after func() }

func (l fillThenLayout) MinSize(objs []fyne.CanvasObject) fyne.Size { return objs[0].MinSize() }

func (l fillThenLayout) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	objs[0].Move(fyne.NewPos(0, 0))
	objs[0].Resize(size)
	l.after()
}
