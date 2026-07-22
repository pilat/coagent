package daemon

import "sync"

// deferAnnouncements is the only thing spanning a deferral episode: the daemon
// rebuilds the session, and its loop, on every wake. In-memory on purpose.
type deferAnnouncements struct {
	mu        sync.Mutex
	bySession map[int64]bool
}

func newDeferAnnouncements() *deferAnnouncements {
	return &deferAnnouncements{bySession: make(map[int64]bool)}
}

func (d *deferAnnouncements) announced(sessionID int64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.bySession[sessionID]
}

// record stores the verdict a run handed back, forgetting the session entirely
// once its episode is over.
func (d *deferAnnouncements) record(sessionID int64, announced bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !announced {
		delete(d.bySession, sessionID)

		return
	}

	d.bySession[sessionID] = true
}
