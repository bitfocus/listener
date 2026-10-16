//go:build !windows

package main

import "syscall"

var (
	errAddrInUse    error = syscall.EADDRINUSE
	errAddrDenied   error = syscall.EACCES
	errAddrNotAvail error = syscall.EADDRNOTAVAIL
)
