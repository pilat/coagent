package sessionstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type CreateSubagentSession struct {
	ProjectID, ParentID, RootID      int64
	AgentType, Model, ReasoningLevel string
}

func CreateSubagentSessionTx(ctx context.Context, tx *sql.Tx, in CreateSubagentSession) (int64, error) {
	if in.ReasoningLevel == "" {
		in.ReasoningLevel = defaultReasoningLevel
	}
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `INSERT INTO sessions (project_id,parent_id,root_id,agent_type,model,reasoning_level,created_at,updated_at)
  SELECT ?,?,?,?,?,?,?,? FROM sessions WHERE id = ? AND killed_at IS NULL AND status NOT IN ('killed','terminating','stopping','stopped')`,
		in.ProjectID, in.ParentID, in.RootID, in.AgentType, in.Model, in.ReasoningLevel, now, now, in.ParentID)
	if err != nil {
		return 0, fmt.Errorf("create subagent session: %w", err)
	}
	if err := requireOneSessionUpdate(result, in.ParentID); err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("subagent session id: %w", err)
	}
	return id, nil
}
