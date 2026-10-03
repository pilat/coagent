package backgroundprocess

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

// overflowMarker is the host-owned terminal marker appended after the cap.
const overflowMarker = "\n[process output truncated at limit]\n"

// Overflow keeps draining while cancellation proceeds; a writer error would
// rely on SIGPIPE instead of recording the terminal intent.
type collector struct {
	file       *os.File
	quota      int64
	ctx        context.Context //nolint:containedctx // Output capture shares the detached process lifetime.
	store      Store
	processID  string
	quotaReady <-chan bool
	cmd        *exec.Cmd
	mu         sync.Mutex
	written    int64
	overflow   bool
	done       bool
}

func newCollector(ctx context.Context, file *os.File, quota int64, store Store, processID string, quotaReady <-chan bool, cmd *exec.Cmd) *collector {
	return &collector{ctx: ctx, file: file, quota: quota, store: store, processID: processID, quotaReady: quotaReady, cmd: cmd}
}

func (c *collector) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.done {
		return len(p), nil
	}

	if c.overflow {
		return len(p), nil
	}

	space := c.quota - c.written
	if int64(len(p)) <= space {
		n, err := c.file.Write(p)
		if err != nil {
			return n, fmt.Errorf("write process output: %w", err)
		}

		c.written += int64(n)

		return n, nil
	}

	if space > 0 {
		if _, err := c.file.Write(p[:space]); err != nil {
			return int(space), fmt.Errorf("write process output: %w", err)
		}
	}

	c.written = c.quota
	c.overflow = true
	c.quotaReached()

	return len(p), nil
}

// Close finishes the file: on overflow the terminal marker is appended and
// every later write is discarded.
func (c *collector) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.done = true

	if c.overflow {
		if _, err := c.file.WriteString(overflowMarker); err != nil {
			_ = c.file.Close()

			return fmt.Errorf("append overflow marker: %w", err)
		}
	}

	if err := c.file.Close(); err != nil {
		return fmt.Errorf("close process output: %w", err)
	}

	return nil
}

// Size reports the persisted byte count.
func (c *collector) Size() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.written
}

func (c *collector) overflowed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.overflow
}

func (c *collector) quotaReached() {
	if persisted := <-c.quotaReady; persisted {
		if _, err := c.store.RecordIntent(c.ctx, c.processID, IntentOutputLimit); err != nil {
			logger.Ctx(c.ctx).Named("backgroundprocess.output").Warn(
				"output_limit_intent_failed", zap.String("process", c.processID), zap.Error(err),
			)
		}
	}
	_ = killGroup(c.cmd)
}
