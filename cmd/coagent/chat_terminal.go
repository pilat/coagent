package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// inputPoll is how long a line read waits before reporting no input. That gap is
// what lets one goroutine own the device and still switch into masked mode.
const inputPoll = 100 * time.Millisecond

// errNoInput means nothing was typed within the poll window. It is not a
// failure: the input loop uses it to look for work other than a chat line.
var errNoInput = errors.New("no input")

var _ terminal = (*ttyTerminal)(nil)

// terminal is the chat's input device: reads go through one owner in one order, so
// a masked prompt is a mode rather than a rival reader of the same descriptor.
type terminal interface {
	// ReadLine returns the next line, or errNoInput when none arrived in time.
	ReadLine() (string, error)
	// ReadSecret returns the next line with echo off, or errNoInput when none
	// completed in time. Echo stays off across calls: the masked prompt is one
	// mode, not one read, which is what lets the loop abandon it.
	ReadSecret() (string, error)
	// EndSecret leaves masked mode without a value, discarding anything typed at
	// the prompt.
	EndSecret()
}

type ttyTerminal struct {
	file   *os.File
	reader *bufio.Reader
	wait   time.Duration

	// inMask is the prompt mode; echo is what the terminal gave up for it, which
	// is nothing at all when the input is not a terminal.
	inMask bool
	echo   echoState
}

func newTTYTerminal(f *os.File) *ttyTerminal {
	return &ttyTerminal{file: f, reader: bufio.NewReaderSize(f, 64<<10), wait: inputPoll}
}

func (t *ttyTerminal) ReadLine() (string, error) {
	if t.reader.Buffered() == 0 {
		ready, err := waitReadable(t.file.Fd(), t.wait)
		if err != nil {
			return "", err
		}

		if !ready {
			return "", errNoInput
		}
	}

	return t.readLine()
}

func (t *ttyTerminal) ReadSecret() (string, error) {
	if !t.inMask {
		state, err := maskEcho(t.file)
		if err != nil {
			return "", err
		}

		t.echo, t.inMask = state, true
	}

	if t.reader.Buffered() == 0 {
		ready, err := waitReadable(t.file.Fd(), t.wait)
		if err != nil {
			t.EndSecret()

			return "", err
		}

		if !ready {
			return "", errNoInput
		}
	}

	line, err := t.readLine()
	t.leaveMask(false)

	return line, err
}

func (t *ttyTerminal) EndSecret() { t.leaveMask(true) }

func (t *ttyTerminal) leaveMask(discard bool) {
	if !t.inMask {
		return
	}

	if discard {
		_, _ = t.reader.Discard(t.reader.Buffered())
	}

	_ = unmaskEcho(t.file, t.echo, discard)
	t.echo, t.inMask = echoState{}, false
}

func (t *ttyTerminal) readLine() (string, error) {
	line, err := t.reader.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || line == "") {
		return "", fmt.Errorf("read input: %w", err)
	}

	return strings.TrimRight(line, "\r\n"), nil
}
