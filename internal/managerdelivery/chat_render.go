package managerdelivery

import (
	"context"
	"fmt"
	"strings"
)

func (w *chatLoop) prepare(ctx context.Context, focus int64) error {
	sessions, err := w.port.Sessions(ctx)
	if err != nil {
		return fmt.Errorf("read chat sessions for entry: %w", err)
	}

	name := ""

	for _, session := range sessions {
		if session.ID == focus {
			name = session.Name
			if session.Scheduled {
				name += " ⏰"
			}

			break
		}
	}

	rows, newest, err := w.port.Entry(ctx, focus, 3)
	if err != nil {
		return fmt.Errorf("read chat entry: %w", err)
	}

	header := strings.TrimSpace(fmt.Sprintf("→ #%d %s", focus, strings.ReplaceAll(name, "*", "")))

	prelude := []chatPreludeItem{{text: "**" + header + "**"}}
	for i := range rows {
		prelude = append(prelude, chatPreludeItem{row: &rows[i]})
	}

	w.prelude, w.preludeFor, w.preludeNewest = prelude, focus, newest

	return nil
}

func chatRowText(focus int64, row ChatRow) string {
	return fmt.Sprintf("**#%d %s**\n\n%s", focus, strings.ReplaceAll(row.Name, "*", ""), row.Content)
}
