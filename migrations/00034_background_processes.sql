-- +goose Up

CREATE TABLE background_processes (
    id TEXT PRIMARY KEY CHECK (id <> ''),
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    root_session_id INTEGER NOT NULL REFERENCES sessions(id),
    tool_call_id TEXT NOT NULL CHECK (tool_call_id <> ''),
    output_path TEXT NOT NULL CHECK (output_path <> ''),
    deadline_at DATETIME NOT NULL,
    created_at DATETIME NOT NULL,
    advertised_at DATETIME,
    output_size INTEGER NOT NULL DEFAULT 0 CHECK (output_size >= 0),
    exit_code INTEGER,
    host_intent TEXT NOT NULL DEFAULT '' CHECK (host_intent IN (
        '', 'deadline', 'output_limit_exceeded',
        'session_stopped', 'session_killed', 'daemon_shutdown'
    )),
    state TEXT NOT NULL DEFAULT 'running' CHECK (state IN (
        'running', 'completed', 'failed', 'timed_out',
        'output_limit_exceeded', 'output_drain_timeout',
        'cancelled', 'interrupted'
    )),
    finished_at DATETIME,
    delivery_state TEXT NOT NULL DEFAULT 'pending' CHECK (delivery_state IN (
        'pending', 'claimed', 'delivered', 'suppressed'
    )),
    delivery_target_session_id INTEGER REFERENCES sessions(id),
    delivered_at DATETIME,
    CHECK ((state = 'running' AND finished_at IS NULL)
           OR (state <> 'running' AND finished_at IS NOT NULL)),
    CHECK (state NOT IN ('completed', 'failed') OR exit_code IS NOT NULL),
    CHECK ((delivery_state = 'pending' AND delivery_target_session_id IS NULL)
           OR (delivery_state <> 'pending' AND delivery_target_session_id IS NOT NULL)),
    CHECK (delivery_state NOT IN ('delivered', 'suppressed') OR delivered_at IS NOT NULL),
    CHECK (delivery_state NOT IN ('pending', 'claimed') OR delivered_at IS NULL),
    CHECK (state <> 'running' OR delivery_state = 'pending'),
    -- drain-timeout and group-kill outcomes have no meaningful exit code.
    CHECK (state NOT IN ('timed_out', 'output_limit_exceeded',
                         'output_drain_timeout', 'cancelled', 'interrupted')
           OR exit_code IS NULL),
    CHECK (host_intent = ''
           OR (host_intent = 'deadline' AND state IN ('running', 'timed_out'))
           OR (host_intent = 'output_limit_exceeded'
               AND state IN ('running', 'output_limit_exceeded'))
           OR (host_intent IN ('session_stopped', 'session_killed')
               AND state IN ('running', 'cancelled'))
           OR (host_intent = 'daemon_shutdown' AND state IN ('running', 'interrupted')))
);

-- +goose StatementBegin
CREATE TRIGGER background_processes_validate_owner
BEFORE INSERT ON background_processes
WHEN NOT EXISTS (
    SELECT 1 FROM sessions owner
    WHERE owner.id = NEW.session_id
      AND ((owner.parent_id = 0 AND NEW.root_session_id = owner.id)
           OR (owner.parent_id <> 0 AND NEW.root_session_id = owner.root_id))
)
BEGIN
    SELECT RAISE(ABORT, 'background process owner/root mismatch');
END;
-- +goose StatementEnd

CREATE INDEX IF NOT EXISTS idx_background_processes_root_running
    ON background_processes(root_session_id)
    WHERE state = 'running';

CREATE INDEX IF NOT EXISTS idx_background_processes_session_running
    ON background_processes(session_id)
    WHERE state = 'running';

CREATE INDEX IF NOT EXISTS idx_background_processes_running
    ON background_processes(state)
    WHERE state = 'running';
