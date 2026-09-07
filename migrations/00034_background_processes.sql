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
    CHECK (state <> 'running' OR finished_at IS NULL),
    CHECK (state NOT IN ('completed', 'failed') OR exit_code IS NOT NULL),
    CHECK (delivery_state = 'pending' OR delivery_target_session_id IS NOT NULL),
    CHECK (delivery_state NOT IN ('delivered', 'suppressed') OR delivered_at IS NOT NULL),
    CHECK (delivery_state <> 'claimed' OR delivered_at IS NULL),
    CHECK (host_intent = '' OR state <> 'completed' OR host_intent NOT IN (''))
);

CREATE INDEX IF NOT EXISTS idx_background_processes_root_running
    ON background_processes(root_session_id)
    WHERE state = 'running';

CREATE INDEX IF NOT EXISTS idx_background_processes_session_running
    ON background_processes(session_id)
    WHERE state = 'running';

CREATE INDEX IF NOT EXISTS idx_background_processes_running
    ON background_processes(state)
    WHERE state = 'running';
