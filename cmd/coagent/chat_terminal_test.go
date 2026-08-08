package main

import (
	"io"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The polled read is what lets one goroutine own the device: it must report an
// empty device instead of blocking, and still deliver every buffered line.
func TestTTYTerminal_ReadLinePollsAndDelivers(t *testing.T) {
	reader, writer := pipeTerminal(t)

	line, err := reader.ReadLine()
	require.ErrorIs(t, err, errNoInput)
	assert.Empty(t, line)

	_, err = writer.WriteString("first\nsecond\n")
	require.NoError(t, err)

	line, err = reader.ReadLine()
	require.NoError(t, err)
	assert.Equal(t, "first", line)

	line, err = reader.ReadLine()
	require.NoError(t, err)
	assert.Equal(t, "second", line)

	line, err = reader.ReadLine()
	require.ErrorIs(t, err, errNoInput)
	assert.Empty(t, line)
}

// A closed input is end of file, which is how Ctrl-D leaves the chat.
func TestTTYTerminal_ReadLineReportsEOF(t *testing.T) {
	reader, writer := pipeTerminal(t)

	_, err := writer.WriteString("trailing")
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	line, err := reader.ReadLine()
	require.NoError(t, err)
	assert.Equal(t, "trailing", line)

	_, err = reader.ReadLine()
	require.ErrorIs(t, err, io.EOF)
}

// Without a terminal there is no echo to turn off, and refusing would strand the
// request — the value still leaves through the same single reader.
func TestTTYTerminal_ReadSecretWithoutATerminal(t *testing.T) {
	reader, writer := pipeTerminal(t)

	_, err := writer.WriteString("sk-piped\n")
	require.NoError(t, err)

	value, err := reader.ReadSecret()
	require.NoError(t, err)
	assert.Equal(t, "sk-piped", value)
}

// The masked read polls like the line read, and the prompt survives the gap: a
// mode that reopened every poll window could not be abandoned from outside.
func TestTTYTerminal_ReadSecretPollsAndHoldsTheMode(t *testing.T) {
	reader, writer := pipeTerminal(t)

	_, err := reader.ReadSecret()
	require.ErrorIs(t, err, errNoInput)
	assert.True(t, reader.inMask, "the prompt stays open between polls")

	_, err = writer.WriteString("sk-polled\n")
	require.NoError(t, err)

	value, err := reader.ReadSecret()
	require.NoError(t, err)
	assert.Equal(t, "sk-polled", value)
	assert.False(t, reader.inMask, "a value ends the prompt")
}

// What was typed at a prompt resolved elsewhere is dropped, never handed on as
// the next chat line.
func TestTTYTerminal_EndSecretDropsWhatWasTypedAtThePrompt(t *testing.T) {
	reader, writer := pipeTerminal(t)

	_, err := reader.ReadSecret()
	require.ErrorIs(t, err, errNoInput)

	_, err = writer.WriteString("half-typed\n")
	require.NoError(t, err)

	// Buffered but not yet claimed by the prompt — the state a dismissal lands in.
	_, err = reader.reader.Peek(1)
	require.NoError(t, err)

	reader.EndSecret()
	assert.False(t, reader.inMask)

	_, err = reader.ReadLine()
	require.ErrorIs(t, err, errNoInput, "the abandoned value is not the next chat line")
}

func pipeTerminal(t *testing.T) (*ttyTerminal, *os.File) {
	t.Helper()

	r, w, err := os.Pipe()
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})

	term := newTTYTerminal(r)
	term.wait = 20 * time.Millisecond

	return term, w
}
