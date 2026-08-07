//go:build darwin

package main

import "golang.org/x/sys/unix"

// The flush variant restores the terminal and drops unread input in one call, so
// a dismissed prompt cannot hand its half-typed line to the next read.
const (
	ioctlReadTermios  = unix.TIOCGETA
	ioctlWriteTermios = unix.TIOCSETA
	ioctlFlushTermios = unix.TIOCSETAF
)
