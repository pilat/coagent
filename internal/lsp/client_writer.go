package lsp

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

type outbound struct {
	data        []byte
	done        chan writeResult
	beforeWrite func() uint64
	state       atomic.Uint32
}

type writeResult struct {
	generation uint64
	err        error
}

const (
	outboundQueued uint32 = iota
	outboundActive
	outboundCanceled
	outboundComplete
)

func (c *client) writeLoop() {
	defer c.recoverWriter()

	var stopped error

	for {
		message, ok := c.nextOutbound()
		if !ok {
			return
		}

		result := writeResult{err: stopped}
		if stopped == nil && message.start() {
			if message.beforeWrite != nil {
				result.generation = message.beforeWrite()
			}

			if err := writeLSPFrame(c.stdin, message.data); err != nil {
				stopped = fmt.Errorf("write LSP frame: %w", err)
				result.err = stopped

				c.failWriter()
				logger.Named("lsp.client").Debug("LSP writer stopped", zap.Error(stopped))
			}

			message.finish()
		} else if stopped == nil {
			result.err = context.Canceled
		}

		message.done <- result
	}
}

func (m *outbound) start() bool { return m.state.CompareAndSwap(outboundQueued, outboundActive) }

func (m *outbound) cancel() bool { return m.state.CompareAndSwap(outboundQueued, outboundCanceled) }

func (m *outbound) finish() { m.state.Store(outboundComplete) }

func (c *client) nextOutbound() (*outbound, bool) {
	select {
	case <-c.writerDone:
		return nil, false
	case message := <-c.writer:
		return message, true
	}
}

func (c *client) failWriter() {
	c.writerStop.Do(func() {
		c.cleanupPending()

		if c.stdin != nil {
			_ = c.stdin.Close()
		}

		if c.cmd != nil && c.cmd.Process != nil {
			c.ensureProcessWaiter()
			_ = c.cmd.Process.Kill()
		}

		if c.writerDone != nil {
			close(c.writerDone)
		}
	})
}

func (c *client) recoverWriter() {
	if recovered := recover(); recovered != nil {
		logger.Named("lsp.client").Error("LSP writer panic", zap.Any("recovered", recovered), zap.Stack("stack"))
		c.failWriter()
	}
}

func writeLSPFrame(writer io.Writer, body []byte) error {
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))
	if err := writeAll(writer, []byte(header)); err != nil {
		return fmt.Errorf("write header: %w", err)
	}

	if err := writeAll(writer, body); err != nil {
		return fmt.Errorf("write body: %w", err)
	}

	return nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return fmt.Errorf("write: %w", err)
		}

		if written == 0 {
			return io.ErrShortWrite
		}

		data = data[written:]
	}

	return nil
}
