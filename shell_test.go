package main

import "testing"

func TestWindowsShellCmdLine(t *testing.T) {
	cases := []string{
		`dir`,
		`echo "hi"`,
		`"C:\Program Files\App\app.exe" --flag`,
		`echo done"`,
		`C:\Users\me\script.bat C:\path\with\trailing\`,
		`echo %USERPROFILE%`,
		`echo hi > "%TEMP%\out.txt"`,
		`echo one & echo two`,
		`echo {"key": "value"} > out.json`,
	}
	for _, shell := range cases {
		want := `cmd.exe /S /C "` + shell + `"`
		if got := windowsShellCmdLine(shell); got != want {
			t.Errorf("windowsShellCmdLine(%q) = %q, want %q", shell, got, want)
		}
	}
}
