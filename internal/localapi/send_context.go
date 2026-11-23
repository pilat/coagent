package localapi

import "context"

func (s sessionService) sendContext(request context.Context) (context.Context, context.CancelFunc) {
	accepted, cancel := context.WithCancel(context.WithoutCancel(request))

	go func() {
		select {
		case <-s.closing:
			cancel()
		case <-accepted.Done():
			return
		}
	}()

	return accepted, cancel
}
