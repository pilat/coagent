package daemon

func (s *svc) removeQueuedSessions(ids []int64) {
	stopped := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		stopped[id] = struct{}{}
	}

	s.childQueue.Remove(func(queued queuedChild) bool {
		_, remove := stopped[queued.sessionID]

		return remove
	})
	s.pendingQueue.Remove(func(queued queuedRunner) bool {
		_, remove := stopped[queued.sessionID]

		return remove
	})
}
