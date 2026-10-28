package assistant

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pilat/coagent/internal/llmwire"
)

func (t *assistantTool) editFocus(ctx context.Context, tx *sql.Tx, call llmwire.ToolCall) (string, error) {
	var args focusArgs
	if err := decodeArgs(call, &args); err != nil {
		return "", err
	}

	item := Focus{Text: strings.TrimSpace(args.Text)}
	if call.Name != toolFocus {
		id, err := noteID(args.ID, "f")
		if err != nil {
			return "", err
		}

		item.ID = id
		var refs string

		err = tx.QueryRowContext(ctx, `SELECT sessions FROM assistant_focus WHERE id = ? AND session_id = ?`, id, t.scope.Root).
			Scan(&refs)
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("no focus item %q", args.ID)
		}

		if err != nil {
			return "", fmt.Errorf("load focus item: %w", err)
		}

		if err := json.Unmarshal([]byte(refs), &item.Sessions); err != nil {
			return "", fmt.Errorf("decode focus sessions: %w", err)
		}
	}

	if call.Name == toolDoneFocus {
		return "removed focus " + args.ID, deleteFocusTx(ctx, tx, t.scope.Root, item.ID)
	}

	if err := checkFocusText(item.Text); err != nil {
		return "", err
	}

	if args.Sessions != nil {
		sessions, err := t.focusSessions(ctx, tx, args.Sessions)
		if err != nil {
			return "", err
		}

		item.Sessions = sessions
	}

	now := time.Now().UTC()
	if call.Name == toolUpdateFocus {
		return "updated focus " + args.ID, updateFocusTx(ctx, tx, t.scope.Root, item, now)
	}

	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM assistant_focus WHERE session_id = ?`, t.scope.Root).
		Scan(&count); err != nil {
		return "", fmt.Errorf("count live focus: %w", err)
	}

	if count >= focusLiveItems {
		return "", fmt.Errorf(
			"focus holds at most %d items: finish or remove some with done_focus first",
			focusLiveItems,
		)
	}

	id, err := insertFocusTx(ctx, tx, t.scope.Root, item, now)

	return fmt.Sprintf("f%d", id), err
}

func (t *assistantTool) focusSessions(ctx context.Context, tx *sql.Tx, refs []sessionRef) ([]string, error) {
	sessions := []string{}

	for _, ref := range refs {
		id, ok := ref.id()
		if !ok {
			return nil, fmt.Errorf("session %q must be a session id", ref)
		}

		authorized, err := authorizedTx(ctx, tx, t.scope.Owner, id)
		if err != nil {
			return nil, err
		}

		if !authorized {
			return nil, fmt.Errorf("session %d is not a work session you can address", id)
		}

		value := strconv.FormatInt(id, 10)
		if !slices.Contains(sessions, value) {
			sessions = append(sessions, value)
		}
	}

	return sessions, nil
}

func checkFocusText(text string) error {
	if text == "" || strings.ContainsAny(text, "\r\n") {
		return errors.New("a focus item needs one line of text")
	}

	if chars := utf8.RuneCountInString(text); chars > focusTextChars {
		return fmt.Errorf("a focus item holds at most %d characters, this one has %d", focusTextChars, chars)
	}

	return nil
}

func noteID(ref, prefix string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(ref), prefix), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid %s id %q", prefix, ref)
	}

	return id, nil
}
