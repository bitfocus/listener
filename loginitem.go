package main

// loginItemStatus is the OS's view of launch at login. The OS is the source of
// truth, since users can also switch the entry off in its own settings.
type loginItemStatus int

const (
	loginItemOff loginItemStatus = iota
	loginItemOn
	// loginItemBlocked means the entry exists but the user switched it off in
	// the OS's settings (macOS: it needs their approval there).
	loginItemBlocked
)
