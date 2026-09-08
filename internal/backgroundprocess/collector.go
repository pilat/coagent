package backgroundprocess

import (
	"fmt"
	"os"
	"sync"
)

// overflowMarker is the host-owned terminal marker appended after the cap.
const overflowMarker = "\n[process output truncated at limit]\n"

// collector merges stdout and stderr into one file-backed bounded stream.
// Writes are serialized so the recorded order is the observed order across
// the two descriptors. Past the byte cap it invokes the overflow callback
// once and keeps discarding: pipe draining must continue while cancellation
// proceeds, and a writer error would rely on SIGPIPE instead.
type collector struct {
	file     *os.File
	quota    int64
	onQuota  func()
	mu       sync.Mutex
	written  int64
	overflow bool
	done     bool
}

func newCollector(file *os.File, quota int64, onQuota func()) *collector {
	return &collector{file: file, quota: quota, onQuota: onQuota}
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
	c.onQuota()

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
