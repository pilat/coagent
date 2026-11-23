package telegram

import "github.com/pilat/coagent/api/coagent/v1"

func isActiveSession(s *coagentv1.SessionDetails) bool {
	return !s.GetKilled() && s.GetStatus() != "terminating" && s.GetStatus() != "killed"
}

func filterActiveSessions(sessions []*coagentv1.SessionDetails) []*coagentv1.SessionDetails {
	active := make([]*coagentv1.SessionDetails, 0, len(sessions))

	for _, s := range sessions {
		if isActiveSession(s) {
			active = append(active, s)
		}
	}

	return active
}

func (m *Manager) setWorkDir(sessionID int64, workDir string) {
	m.mu.Lock()
	m.workDirs[sessionID] = workDir
	m.mu.Unlock()
}
