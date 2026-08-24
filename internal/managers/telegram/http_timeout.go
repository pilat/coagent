package telegram

import (
	"errors"
	"math"
	"time"
)

func httpTimeoutFor(pollTimeoutSec int) (time.Duration, error) {
	const (
		minTimeout = 45 * time.Second
		margin     = 15 * time.Second
	)

	if pollTimeoutSec < 0 {
		return 0, errors.New("poll_timeout_sec must be >= 0")
	}

	maxSeconds := math.MaxInt64 / int64(time.Second)
	if int64(pollTimeoutSec) > maxSeconds-int64(margin/time.Second) {
		return 0, errors.New("poll_timeout_sec overflows time.Duration")
	}

	timeout := time.Duration(pollTimeoutSec)*time.Second + margin
	if timeout < minTimeout {
		return minTimeout, nil
	}

	return timeout, nil
}
