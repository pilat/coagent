package managerdelivery

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

func (w *chatLoop) prepare(ctx context.Context, focus int64) error {
	enter := w.enterTarget.Load() == focus
	if !w.initialized && !enter {
		w.initialized, w.lastFocus = true, focus
		return nil
	}

	if w.initialized && focus == w.lastFocus && !enter {
		return nil
	}

	sessions, err := w.port.Sessions(ctx)
	if err != nil {
		return fmt.Errorf("read chat sessions for switch: %w", err)
	}

	name := ""
	contextEligible := false
	scheduled := false

	for _, session := range sessions {
		if session.ID == focus {
			name = session.Name
			contextEligible = session.Unread == 0
			scheduled = session.Scheduled

			break
		}
	}

	if focus == 0 {
		name = "Assistant"
	}

	if scheduled {
		name += " ⏰"
	}

	prelude := []chatPreludeItem{{text: strings.TrimSpace(fmt.Sprintf("→ #%d %s", focus, name))}}
	if focus == 0 {
		if digest := unreadDigest(sessions, focus); digest != "" {
			prelude = append(prelude, chatPreludeItem{text: digest})
		}
	}

	if contextEligible {
		row, found, err := w.port.LastSeen(ctx, focus)
		if err != nil {
			return fmt.Errorf("read entry context: %w", err)
		}

		if found {
			prelude = append(prelude, chatPreludeItem{text: chatRowText(focus, row), dropOnRowPermanent: true})
		}
	}

	w.prelude = prelude
	w.initialized, w.lastFocus = true, focus
	w.enterTarget.CompareAndSwap(focus, -1)

	return nil
}

func chatRowText(focus int64, row ChatRow) string {
	if focus == 0 {
		return row.Content
	}

	return fmt.Sprintf("#%d %s\n%s", focus, row.Name, row.Content)
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
