-- +goose Up

-- SQLite cannot extend a CHECK constraint in place. Preserve inbox identities so
-- accepted-message references and FIFO ordering remain valid across the rebuild.
CREATE TEMP TABLE session_inbox_sequence_00036 (seq INTEGER NOT NULL);
INSERT INTO session_inbox_sequence_00036
SELECT seq FROM sqlite_sequence WHERE name = 'session_inbox';
DROP INDEX idx_session_tool_activations_pending;
ALTER TABLE session_tool_activations RENAME TO session_tool_activations_legacy_00036;
DROP INDEX idx_session_inbox_pending_fifo;
DROP INDEX idx_session_inbox_accepted_session;
ALTER TABLE session_inbox RENAME TO session_inbox_legacy_00036;

CREATE TABLE session_inbox (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    source TEXT NOT NULL CHECK (source IN ('user', 'agent', 'process', 'subagent')),
    raw_content TEXT NOT NULL CHECK (raw_content <> ''),
    received_at DATETIME NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'accepted', 'handled', 'rejected', 'cancelled')),
    resolved_at DATETIME,
    resolution_reason TEXT,
    accepted_message_id INTEGER REFERENCES messages(id),
    attributes TEXT NOT NULL DEFAULT '{}'
        CHECK (json_valid(attributes) AND json_type(attributes) = 'object'),
    CHECK (
        (state = 'pending'
            AND resolved_at IS NULL
            AND resolution_reason IS NULL
            AND accepted_message_id IS NULL)
        OR (state = 'accepted'
            AND resolved_at IS NOT NULL
            AND resolution_reason IS NULL
            AND accepted_message_id IS NOT NULL)
        OR (state IN ('handled', 'rejected', 'cancelled')
            AND resolved_at IS NOT NULL
            AND resolution_reason IS NOT NULL
            AND resolution_reason <> ''
            AND accepted_message_id IS NULL)
    )
);

INSERT INTO session_inbox
    (id, session_id, source, raw_content, received_at, state, resolved_at,
     resolution_reason, accepted_message_id, attributes)
SELECT id, session_id, source, raw_content, received_at, state, resolved_at,
       resolution_reason, accepted_message_id, attributes
FROM session_inbox_legacy_00036;

CREATE TABLE session_tool_activations (
    input_id INTEGER PRIMARY KEY REFERENCES session_inbox(id),
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    tool_id TEXT NOT NULL CHECK (tool_id <> ''),
    command TEXT NOT NULL CHECK (command <> '' AND substr(command, 1, 1) = '/'),
    state TEXT NOT NULL CHECK (state IN ('pending', 'consumed', 'expired')),
    tool_call_id TEXT,
    created_at DATETIME NOT NULL,
    resolved_at DATETIME,
    CHECK (
        (state = 'pending' AND tool_call_id IS NULL AND resolved_at IS NULL)
        OR (state = 'consumed' AND tool_call_id IS NOT NULL AND tool_call_id <> '' AND resolved_at IS NOT NULL)
        OR (state = 'expired' AND tool_call_id IS NULL AND resolved_at IS NOT NULL)
    )
);

INSERT INTO session_tool_activations
    (input_id, session_id, tool_id, command, state, tool_call_id, created_at, resolved_at)
SELECT input_id, session_id, tool_id, command, state, tool_call_id, created_at, resolved_at
FROM session_tool_activations_legacy_00036;

INSERT INTO sqlite_sequence (name, seq)
SELECT 'session_inbox', seq FROM session_inbox_sequence_00036
WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = 'session_inbox');
UPDATE sqlite_sequence
SET seq = MAX(seq, COALESCE((SELECT seq FROM session_inbox_sequence_00036), seq))
WHERE name = 'session_inbox';
DROP TABLE session_inbox_sequence_00036;

DROP TABLE session_tool_activations_legacy_00036;
DROP TABLE session_inbox_legacy_00036;

CREATE UNIQUE INDEX idx_session_tool_activations_pending
    ON session_tool_activations(session_id) WHERE state = 'pending';
CREATE INDEX idx_session_inbox_pending_fifo
    ON session_inbox(session_id, id) WHERE state = 'pending';
CREATE INDEX idx_session_inbox_accepted_session
    ON session_inbox(session_id, id)
    WHERE state = 'accepted' AND accepted_message_id IS NOT NULL;

UPDATE session_inbox
SET state = 'cancelled', resolved_at = CURRENT_TIMESTAMP, resolution_reason = 'killed'
WHERE state = 'pending' AND session_id IN (
    SELECT member.id FROM sessions member
    JOIN sessions root ON root.id = CASE
        WHEN member.parent_id = 0 THEN member.id ELSE member.root_id
    END
    WHERE member.killed_at IS NOT NULL OR member.status IN ('terminating', 'killed')
       OR root.killed_at IS NOT NULL OR root.status IN ('terminating', 'killed')
);

DROP TRIGGER background_processes_validate_owner;
DROP INDEX idx_background_processes_root_running;
DROP INDEX idx_background_processes_session_running;
DROP INDEX idx_background_processes_running;
ALTER TABLE background_processes RENAME TO background_processes_legacy_00036;

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
    CHECK ((state = 'running' AND finished_at IS NULL)
           OR (state <> 'running' AND finished_at IS NOT NULL)),
    CHECK (state NOT IN ('completed', 'failed') OR exit_code IS NOT NULL),
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

INSERT INTO background_processes
    (id, session_id, root_session_id, tool_call_id, output_path, deadline_at,
     created_at, advertised_at, output_size, exit_code, host_intent, state, finished_at)
SELECT id, session_id, root_session_id, tool_call_id, output_path, deadline_at,
       created_at, advertised_at, output_size, exit_code, host_intent, state, finished_at
FROM background_processes_legacy_00036;

-- A legacy claimed record alone is not proof that its transcript pair exists.
-- Keep any proven historical delivery; all other owed facts become FIFO input.
WITH owed AS (
    SELECT p.*,
           COALESCE(CAST(ROUND(MAX(0, (
               COALESCE(
                   julianday(CAST(COALESCE(p.finished_at, p.created_at) AS TEXT)),
                   julianday(substr(CAST(COALESCE(p.finished_at, p.created_at) AS TEXT), 1, 19))
               ) - COALESCE(
                   julianday(CAST(p.created_at AS TEXT)),
                   julianday(substr(CAST(p.created_at AS TEXT), 1, 19))
               )
           ) * 86400000.0)) AS INTEGER), 0) AS duration_ms,
           replace(replace(replace(replace(replace(p.id,
               '&', '&amp;'), '<', '&lt;'), '>', '&gt;'), '"', '&#34;'), '''', '&#39;') AS escaped_id,
           replace(replace(replace(replace(replace(p.output_path,
               '&', '&amp;'), '<', '&lt;'), '>', '&gt;'), '"', '&#34;'), '''', '&#39;') AS escaped_path
    FROM background_processes_legacy_00036 p
    JOIN sessions owner ON owner.id = p.session_id
    WHERE p.advertised_at IS NOT NULL
      AND p.state <> 'running'
      AND p.delivery_state IN ('pending', 'claimed')
      AND p.host_intent NOT IN ('session_stopped', 'session_killed')
      AND owner.killed_at IS NULL
      AND owner.status NOT IN ('terminating', 'killed')
      AND NOT EXISTS (
          SELECT 1 FROM session_deliveries d
          WHERE d.session_id = p.delivery_target_session_id AND d.delivery_id = p.id
      )
), formatted AS (
    SELECT owed.*,
           CASE
               WHEN duration_ms = 0 THEN '0s'
               WHEN duration_ms < 1000 THEN CAST(duration_ms AS TEXT) || 'ms'
               WHEN duration_ms < 60000 THEN
                   rtrim(rtrim(printf('%.3f', duration_ms / 1000.0), '0'), '.') || 's'
               WHEN duration_ms < 3600000 THEN
                   CAST(duration_ms / 60000 AS TEXT) || 'm' ||
                   rtrim(rtrim(printf('%.3f', (duration_ms % 60000) / 1000.0), '0'), '.') || 's'
               ELSE CAST(duration_ms / 3600000 AS TEXT) || 'h' ||
                   CAST((duration_ms % 3600000) / 60000 AS TEXT) || 'm' ||
                   rtrim(rtrim(printf('%.3f', (duration_ms % 60000) / 1000.0), '0'), '.') || 's'
           END AS duration
    FROM owed
)
INSERT INTO session_inbox (session_id, source, raw_content, attributes, received_at)
SELECT p.session_id, 'process',
       '<process_completion>' || char(10) || 'process_id: ' || p.escaped_id || char(10) || 'state: ' || p.state ||
       char(10) || 'exit_code: ' || COALESCE(CAST(p.exit_code AS TEXT), 'unavailable') ||
       char(10) || 'duration: ' || p.duration ||
       char(10) || 'origin_session_id: ' || p.session_id || char(10) || 'output_file: ' || p.escaped_path ||
       char(10) || 'preview_status: migration_unavailable' || char(10) || '</process_completion>',
       json_object('process_id', p.id),
       COALESCE(
           datetime(CAST(COALESCE(p.finished_at, p.created_at) AS TEXT)),
           datetime(substr(CAST(COALESCE(p.finished_at, p.created_at) AS TEXT), 1, 19)),
           '1970-01-01 00:00:00'
       )
FROM formatted p;

DROP TABLE background_processes_legacy_00036;

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

CREATE INDEX idx_background_processes_root_running
    ON background_processes(root_session_id) WHERE state = 'running';
CREATE INDEX idx_background_processes_session_running
    ON background_processes(session_id) WHERE state = 'running';
CREATE INDEX idx_background_processes_running
    ON background_processes(state) WHERE state = 'running';

ALTER TABLE subagent_links
    ADD COLUMN delivered_input_id INTEGER REFERENCES session_inbox(id);
