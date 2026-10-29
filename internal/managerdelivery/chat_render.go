package managerdelivery

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

func (w *chatLoop) prepare(ctx context.Context, focus int64) error {
	if !w.initialized {
		w.initialized, w.lastFocus = true, focus
		return nil
	}

	if focus == w.lastFocus {
		return nil
	}

	sessions, err := w.port.Sessions(ctx)
	if err != nil {
		return fmt.Errorf("read chat sessions for switch: %w", err)
	}

	name := ""

	for _, session := range sessions {
		if session.ID == focus {
			name = session.Name
			break
		}
	}

	if focus == 0 {
		name = "Assistant"
	}

	w.prelude = []string{strings.TrimSpace(fmt.Sprintf("→ #%d %s", focus, name))}
	if digest := unreadDigest(sessions, focus); digest != "" {
		w.prelude = append(w.prelude, digest)
	}

	w.lastFocus = focus

	return nil
}

func unreadDigest(sessions []ChatSession, focus int64) string {
	sort.SliceStable(sessions, func(i, j int) bool {
		return sessions[i].LatestUnreadID > sessions[j].LatestUnreadID
	})

	var lines []string

	for _, session := range sessions {
		if session.ID == focus || session.Unread == 0 {
			continue
		}

		name := session.Name
		if session.ID == 0 {
			name = "Assistant"
		}

		first, _, _ := strings.Cut(session.LatestUnread, "\n")
		runes := []rune(first)

		if len(runes) > 120 {
			first = string(runes[:120])
		}

		lines = append(lines, fmt.Sprintf("#%d %s · %d new · %s", session.ID, name, session.Unread, first))
	}

	return strings.Join(lines, "\n")
}
