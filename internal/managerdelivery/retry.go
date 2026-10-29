package managerdelivery

import "time"

const (
	baseRetry = 3 * time.Second
	maxRetry  = 60 * time.Second
)

func retryDelay(attempts int64, retryAfter time.Duration) time.Duration {
	delay := baseRetry
	for i := int64(1); i < attempts && delay < maxRetry; i++ {
		delay *= 2
	}

	return max(min(delay, maxRetry), retryAfter)
}
