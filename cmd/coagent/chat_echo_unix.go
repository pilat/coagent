//go:build linux || darwin

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// echoState is what maskEcho took away, so unmaskEcho puts back exactly that and
// not a guess at what the terminal looked like before.
type echoState struct {
	masked  bool
	termios unix.Termios
}

// maskEcho turns echo off for the length of a masked prompt. Canonical mode stays
// on, so the kernel keeps doing line editing and signals — and a readable
// descriptor still means one complete line, which is what makes the read pollable.
func maskEcho(f *os.File) (echoState, error) {
	fd := int(f.Fd())
	if !term.IsTerminal(fd) {
		return echoState{}, nil
	}

	saved, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
	if err != nil {
		return echoState{}, fmt.Errorf("read terminal mode: %w", err)
	}

	masked := *saved
	masked.Lflag &^= unix.ECHO
	masked.Lflag |= unix.ICANON | unix.ISIG

	if err := unix.IoctlSetTermios(fd, ioctlWriteTermios, &masked); err != nil {
		return echoState{}, fmt.Errorf("mask terminal echo: %w", err)
	}

	return echoState{masked: true, termios: *saved}, nil
}

// unmaskEcho ends masked mode. discard also drops what was typed but never
// submitted: a credential half-typed at a dismissed prompt is not chat text.
func unmaskEcho(f *os.File, state echoState, discard bool) error {
	if !state.masked {
		return nil
	}

	req := uint(ioctlWriteTermios)
	if discard {
		req = uint(ioctlFlushTermios)
	}

	if err := unix.IoctlSetTermios(int(f.Fd()), req, &state.termios); err != nil {
		return fmt.Errorf("restore terminal echo: %w", err)
	}

	return nil
}
