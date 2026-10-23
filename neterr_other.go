//go:build !windows

package main

import "syscall"

var (
	errAddrInUse    error = syscall.EADDRINUSE
	errAddrDenied   error = syscall.EACCES
	errAddrNotAvail error = syscall.EADDRNOTAVAIL
)

// errFileInUse is nil here: only Windows locks files that other programs have open.
var errFileInUse error
