package main

import "syscall"

// Winsock reports listen failures with its own WSA codes rather than POSIX errnos.
var (
	errAddrInUse    error = syscall.Errno(10048) // WSAEADDRINUSE
	errAddrDenied   error = syscall.Errno(10013) // WSAEACCES
	errAddrNotAvail error = syscall.Errno(10049) // WSAEADDRNOTAVAIL
)

// errFileInUse is ERROR_SHARING_VIOLATION, returned when another program
// (typically Excel) holds the file open.
var errFileInUse error = syscall.Errno(32)
