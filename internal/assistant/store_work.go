package assistant

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/pilat/coagent/internal/tool"
)

const (
	// workRootSQL is the single authority predicate over `sessions s`; its one placeholder binds the owner.
	workRootSQL = `json_extract(s.attributes, '$.manager_id') = ? AND COALESCE(s.parent_id, 0) = 0
		AND COALESCE(s.agent_type, '') <> '` + tool.AssistantAgentType + `'
		AND s.killed_at IS NULL AND COALESCE(s.status, '') <> 'killed'
		AND (COALESCE(json_type(s.attributes, '$.management_surface'), 'null') IN ('null', 'false')
			OR (json_type(s.attributes, '$.management_surface') = 'text'
				AND json_extract(s.attributes, '$.management_surface') IN ('', 'false', '0')))`
	aboutFallbackChars = 120
)

// WorkRoot is a session the assistant may address, before the daemon adds live status and places.
type WorkRoot struct {
	ID        int64
	ProjectID int64
	Status    string
	CreatedAt time.Time
	Activity  time.Time
	Context   int
	Pending   bool
	Touched   bool
	About     string
}

// Authorized applies the same predicate as search and listings to one session.
func (s *store) Authorized(ctx context.Context, owner string, id int64) (bool, error) {
	var ok bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions s WHERE s.id = ? AND `+workRootSQL+`)`,
		id, owner).Scan(&ok); err != nil {
		return false, fmt.Errorf("authorize assistant target %d: %w", id, err)
	}

	return ok, nil
}

// WorkRoots lists every authorized root, or only those this assistant sent to.
func (s *store) WorkRoots(ctx context.Context, scope Scope, touchedOnly bool) ([]WorkRoot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.id, s.project_id, s.status, s.created_at, s.updated_at,
			COALESCE(s.context_baseline_prompt_tokens, 0),
			EXISTS(SELECT 1 FROM assistant_sends a
				WHERE a.session_id = s.id AND a.assistant_session_id = ? AND a.observed_at IS NULL),
			EXISTS(SELECT 1 FROM assistant_sends a WHERE a.session_id = s.id AND a.assistant_session_id = ?),
			o.created_at, w.about, i.raw_content
		FROM sessions s
		LEFT JOIN session_outbox o ON o.id = (SELECT id FROM session_outbox WHERE session_id = s.id
			AND type = 'message_persistent' AND source_key LIKE 'message:%' ORDER BY id DESC LIMIT 1)
		LEFT JOIN assistant_work w ON w.session_id = s.id
		LEFT JOIN session_inbox i ON i.id = (SELECT id FROM session_inbox WHERE session_id = s.id
			AND source = 'user' AND state = 'accepted' ORDER BY id LIMIT 1)
		WHERE `+workRootSQL, scope.Root, scope.Root, scope.Owner)
	if err != nil {
		return nil, fmt.Errorf("load assistant work roots: %w", err)
	}
	defer rows.Close()

	var roots []WorkRoot

	for rows.Next() {
		root, err := scanWorkRoot(rows)
		if err != nil {
			return nil, err
		}

		if root.Touched || !touchedOnly {
			roots = append(roots, root)
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read assistant work roots: %w", err)
	}

	return roots, nil
}

func scanWorkRoot(rows *sql.Rows) (WorkRoot, error) {
	var (
		root             WorkRoot
		status           sql.NullString
		created, updated sql.NullTime
		final            sql.NullTime
		about, asked     sql.NullString
	)

	if err := rows.Scan(&root.ID, &root.ProjectID, &status, &created, &updated, &root.Context,
		&root.Pending, &root.Touched, &final, &about, &asked); err != nil {
		return WorkRoot{}, fmt.Errorf("scan assistant work root: %w", err)
	}

	root.Status, root.CreatedAt, root.Activity = status.String, created.Time, updated.Time
	if final.Time.After(root.Activity) {
		root.Activity = final.Time
	}

	root.About = about.String
	if !about.Valid {
		root.About = Clip(strings.Join(strings.Fields(asked.String), " "), aboutFallbackChars)
	}

	return root, nil
}
