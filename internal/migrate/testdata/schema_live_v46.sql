CREATE TABLE goose_db_version (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		version_id INTEGER NOT NULL,
		is_applied INTEGER NOT NULL,
		tstamp TIMESTAMP DEFAULT (datetime('now'))
	);
CREATE TABLE "messages" (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id INTEGER NOT NULL REFERENCES sessions(id),
		role TEXT NOT NULL,
		content TEXT,
		tool_call_id TEXT,
		tool_name TEXT,
		tool_calls TEXT,
		reasoning_content TEXT,
		cost_usd REAL DEFAULT 0,
		usage TEXT,
		compacted_at DATETIME,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	, position INTEGER, reasoning_raw TEXT, attachments TEXT, tool_error BOOLEAN NOT NULL DEFAULT 0, finish_type TEXT
    CHECK (finish_type IS NULL OR finish_type IN ('stop', 'length', 'tool_calls', 'unknown')), provider_finish_reason TEXT, rejected_reason TEXT
	CHECK (rejected_reason IS NULL OR (
		role = 'assistant' AND finish_type IS NOT NULL AND (
            (finish_type = 'length' AND rejected_reason = 'output_length') OR
            (finish_type = 'unknown' AND rejected_reason = 'unknown_finish')
        )
    )), retry_of_message_id INTEGER REFERENCES messages(id)
    CHECK (retry_of_message_id IS NULL OR role = 'user'));
CREATE TABLE "schedules" (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id INTEGER NOT NULL REFERENCES sessions(id),
			cron_expr TEXT,
			one_shot_at DATETIME,
			input_message TEXT NOT NULL,
			last_fired_at DATETIME,
			metadata TEXT DEFAULT '{}',
			fire_count INTEGER DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP, fresh INTEGER NOT NULL DEFAULT 0,
			CONSTRAINT chk_trigger CHECK (cron_expr IS NOT NULL OR one_shot_at IS NOT NULL)
		);
CREATE TABLE "memories" (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL DEFAULT 0 REFERENCES projects(id),
			text TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
CREATE TABLE "projects" (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    work_dir TEXT NOT NULL,
    name TEXT NOT NULL DEFAULT '', hidden BOOLEAN NOT NULL DEFAULT FALSE);
CREATE TABLE mcp_servers (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id INTEGER REFERENCES projects(id),
    name TEXT NOT NULL,
    command TEXT NOT NULL,
    args TEXT NOT NULL DEFAULT '[]',
    env TEXT NOT NULL DEFAULT '{}',
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);
CREATE TABLE session_deliveries (
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    delivery_id TEXT NOT NULL CHECK (delivery_id <> ''),
    kind TEXT NOT NULL CHECK (kind IN ('tool_notification', 'context_reset')),
    fingerprint TEXT NOT NULL CHECK (fingerprint <> ''),
    delivered_at DATETIME NOT NULL,
    PRIMARY KEY (session_id, delivery_id)
);
CREATE TABLE "sessions" (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id INTEGER NOT NULL DEFAULT 0 REFERENCES projects(id),
    model TEXT,
    reasoning_level TEXT NOT NULL DEFAULT 'medium',
    attributes TEXT DEFAULT '{}',
    agent_type TEXT NOT NULL,
    parent_id INTEGER DEFAULT 0,
    iteration INTEGER DEFAULT 0,
    status TEXT DEFAULT 'active',
    todo_items TEXT DEFAULT '[]',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    killed_at DATETIME,
    root_id INTEGER NOT NULL DEFAULT 0
, episode_started_at DATETIME, model_input_generation INTEGER NOT NULL DEFAULT 0
    CHECK (model_input_generation >= 0), model_input_boundary INTEGER, context_baseline_model TEXT NOT NULL DEFAULT '', context_baseline_prompt_tokens INTEGER NOT NULL DEFAULT 0
    CHECK (context_baseline_prompt_tokens >= 0), context_baseline_message_count INTEGER NOT NULL DEFAULT 0
    CHECK (context_baseline_message_count >= 0), completion_check_candidate_id INTEGER
    REFERENCES messages(id), manager_reply_pending BOOLEAN NOT NULL DEFAULT FALSE, empty_stop_streak INTEGER NOT NULL DEFAULT 0
    CHECK (empty_stop_streak >= 0), completion_check_confirmed_answer_id INTEGER
    REFERENCES messages(id), master_enabled BOOLEAN DEFAULT FALSE, completion_nudge_generation INTEGER);
CREATE TABLE session_outbox (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    type TEXT NOT NULL CHECK (type IN (
        'message_replaceable', 'message_persistent', 'session_opened',
        'session_replaced', 'session_closed'
    )),
    content TEXT NOT NULL,
    attributes TEXT NOT NULL CHECK (json_valid(attributes) AND json_type(attributes) = 'object'),
    source_key TEXT,
    fingerprint TEXT,
    state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN (
        'pending', 'delivering', 'retry_wait', 'delivered', 'blocked'
    )),
    attempt_seq INTEGER NOT NULL DEFAULT 0 CHECK (attempt_seq >= 0),
    attempt_id TEXT,
    last_attempt_at DATETIME,
    next_attempt_at DATETIME,
    delivered_at DATETIME,
    blocked_at DATETIME,
    last_error TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL, releases_input INTEGER NOT NULL DEFAULT 0
    CHECK (releases_input IN (0, 1)),
    CHECK ((source_key IS NULL AND fingerprint IS NULL) OR
           (source_key IS NOT NULL AND source_key <> '' AND fingerprint IS NOT NULL AND fingerprint <> '')),
    CHECK (
        (state = 'pending' AND attempt_seq = 0 AND attempt_id IS NULL
            AND last_attempt_at IS NULL AND next_attempt_at IS NULL AND delivered_at IS NULL
            AND blocked_at IS NULL AND last_error = '')
        OR (state = 'delivering' AND attempt_seq > 0 AND attempt_id IS NOT NULL AND attempt_id <> ''
            AND last_attempt_at IS NOT NULL AND next_attempt_at IS NULL AND delivered_at IS NULL
            AND blocked_at IS NULL AND last_error = '')
        OR (state = 'retry_wait' AND attempt_seq > 0 AND attempt_id IS NULL
            AND last_attempt_at IS NOT NULL AND next_attempt_at IS NOT NULL AND delivered_at IS NULL
            AND blocked_at IS NULL AND last_error <> '')
        OR (state = 'delivered' AND attempt_seq > 0 AND attempt_id IS NULL
            AND last_attempt_at IS NOT NULL AND next_attempt_at IS NULL AND delivered_at IS NOT NULL
            AND blocked_at IS NULL AND last_error = '')
        OR (state = 'blocked' AND attempt_seq > 0 AND attempt_id IS NULL
            AND last_attempt_at IS NOT NULL AND next_attempt_at IS NULL AND delivered_at IS NULL
            AND blocked_at IS NOT NULL AND last_error <> '')
    )
);
CREATE TABLE manager_bindings (
    manager_id TEXT PRIMARY KEY CHECK (manager_id <> ''),
    driver TEXT NOT NULL CHECK (driver <> ''),
    attributes TEXT NOT NULL CHECK (json_valid(attributes) AND json_type(attributes) = 'object' AND attributes <> '{}'),
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL
);
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
        'session_stopped', 'session_killed', 'agent_cancelled', 'daemon_shutdown'
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
           OR (host_intent IN ('session_stopped', 'session_killed', 'agent_cancelled')
               AND state IN ('running', 'cancelled'))
           OR (host_intent = 'daemon_shutdown' AND state IN ('running', 'interrupted')))
);
CREATE TABLE session_file_reads (
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    path TEXT NOT NULL,
    mtime_unix_nano INTEGER NOT NULL,
    size INTEGER NOT NULL,
    hash TEXT NOT NULL,
    updated_at DATETIME NOT NULL,
    PRIMARY KEY (session_id, path)
);
CREATE TABLE session_budgets (
    root_session_id INTEGER PRIMARY KEY REFERENCES sessions(id),
    state TEXT NOT NULL CHECK (state IN ('armed', 'fired', 'released')),
    generation INTEGER NOT NULL CHECK (generation > 0),
    armed_at DATETIME NOT NULL,
    baseline_cost_usd REAL NOT NULL
        CHECK (baseline_cost_usd >= 0 AND baseline_cost_usd <= 1000000000000),
    cost_limit_usd REAL
        CHECK (cost_limit_usd > 0 AND cost_limit_usd <= 1000000),
    duration_seconds INTEGER
        CHECK (duration_seconds BETWEEN 60 AND 31536000),
    fired_at DATETIME,
    released_at DATETIME,
    fired_reason TEXT NOT NULL DEFAULT '' CHECK (fired_reason IN ('', 'cost', 'duration')),
    released_reason TEXT NOT NULL DEFAULT '' CHECK (released_reason IN (
        '', 'cleared', 'resumed', 'completed', 'error', 'stopped', 'killed', 'replaced'
    )),
    observed_cost_usd REAL
        CHECK (observed_cost_usd >= 0 AND observed_cost_usd <= 1000000000000),
    park_phase TEXT NOT NULL DEFAULT '' CHECK (park_phase IN ('', 'requested', 'draining', 'parked')),
    park_owner TEXT NOT NULL DEFAULT '',
    CHECK (
        (state = 'armed'
            AND (cost_limit_usd IS NOT NULL OR duration_seconds IS NOT NULL)
            AND fired_at IS NULL AND released_at IS NULL
            AND fired_reason = '' AND released_reason = '' AND observed_cost_usd IS NULL
            AND park_phase = '' AND park_owner = '')
        OR (state = 'fired'
            AND (cost_limit_usd IS NOT NULL OR duration_seconds IS NOT NULL)
            AND fired_at IS NOT NULL AND released_at IS NULL
            AND fired_reason <> '' AND released_reason = '' AND observed_cost_usd IS NOT NULL
            AND park_phase <> ''
            AND ((park_phase IN ('requested', 'draining') AND park_owner <> '')
                OR (park_phase = 'parked' AND park_owner = '')))
        OR (state = 'released'
            AND released_at IS NOT NULL AND released_reason <> ''
            AND park_owner = '')
    )
);
CREATE TABLE session_inbox (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    source TEXT NOT NULL CHECK (source IN ('user', 'agent', 'process', 'subagent', 'schedule', 'call_result')),
    raw_content TEXT NOT NULL CHECK (raw_content <> ''),
    received_at DATETIME NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'accepted', 'handled', 'rejected', 'cancelled')),
    resolved_at DATETIME,
    resolution_reason TEXT,
    accepted_message_id INTEGER REFERENCES messages(id),
    attributes TEXT NOT NULL DEFAULT '{}'
        CHECK (json_valid(attributes) AND json_type(attributes) = 'object'),
    delivery_key TEXT,
    CHECK (
        (state = 'pending' AND resolved_at IS NULL AND resolution_reason IS NULL AND accepted_message_id IS NULL)
        OR (state = 'accepted' AND resolved_at IS NOT NULL AND resolution_reason IS NULL AND accepted_message_id IS NOT NULL)
        OR (state IN ('handled', 'rejected', 'cancelled') AND resolved_at IS NOT NULL
            AND resolution_reason IS NOT NULL AND resolution_reason <> '' AND accepted_message_id IS NULL)
    )
);
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
CREATE TABLE subagent_links (
    parent_id INTEGER NOT NULL,
    child_id INTEGER NOT NULL,
    task_call_id TEXT NOT NULL,
    blocking INTEGER NOT NULL DEFAULT 0,
    depth INTEGER NOT NULL DEFAULT 0,
    state TEXT NOT NULL DEFAULT 'spawned',
    delivered_at INTEGER,
    delivered_msg_id INTEGER,
    created_at INTEGER NOT NULL,
    result TEXT NOT NULL DEFAULT '',
    outcome TEXT NOT NULL DEFAULT '',
    activation_seq INTEGER NOT NULL DEFAULT 1,
    delivered_input_id INTEGER REFERENCES session_inbox(id),
    PRIMARY KEY (parent_id, child_id)
);
CREATE UNIQUE INDEX idx_projects_workdir ON projects(work_dir);
CREATE UNIQUE INDEX idx_mcp_servers_project_name ON mcp_servers(project_id, name) WHERE project_id IS NOT NULL;
CREATE UNIQUE INDEX idx_mcp_servers_global_name ON mcp_servers(name) WHERE project_id IS NULL;
CREATE UNIQUE INDEX idx_session_outbox_source_key
    ON session_outbox(session_id, source_key)
    WHERE source_key IS NOT NULL;
CREATE UNIQUE INDEX idx_sessions_management_root
    ON sessions(project_id, json_extract(attributes, '$.manager_id'))
    WHERE parent_id = 0
      AND killed_at IS NULL
      AND status NOT IN ('terminating', 'killed')
      AND json_extract(attributes, '$.management_surface') IS NOT NULL;
CREATE INDEX idx_messages_session_active ON messages(session_id) WHERE compacted_at IS NULL;
CREATE INDEX idx_memories_project_id ON memories(project_id);
CREATE INDEX idx_sessions_project_id ON sessions(project_id);
CREATE INDEX idx_session_outbox_manager_head
    ON session_outbox(json_extract(attributes, '$.manager_id'), id)
    WHERE state <> 'delivered';
CREATE INDEX idx_session_outbox_delivered_message
    ON session_outbox(session_id, id DESC)
    WHERE state = 'delivered' AND type IN ('message_replaceable', 'message_persistent');
CREATE INDEX idx_messages_session_history
    ON messages(session_id, id);
CREATE INDEX idx_sessions_root_history
    ON sessions(root_id, id);
CREATE INDEX idx_background_processes_root_running
    ON background_processes(root_session_id) WHERE state = 'running';
CREATE INDEX idx_background_processes_session_running
    ON background_processes(session_id) WHERE state = 'running';
CREATE INDEX idx_background_processes_running
    ON background_processes(state) WHERE state = 'running';
CREATE UNIQUE INDEX idx_session_tool_activations_pending
    ON session_tool_activations(session_id) WHERE state = 'pending';
CREATE INDEX idx_subagent_links_undelivered
    ON subagent_links(parent_id) WHERE delivered_at IS NULL;
CREATE INDEX idx_subagent_links_child ON subagent_links(child_id);
CREATE INDEX idx_session_inbox_pending_fifo
    ON session_inbox(session_id, id) WHERE state = 'pending';
CREATE INDEX idx_session_inbox_accepted_session
    ON session_inbox(session_id, id) WHERE state = 'accepted' AND accepted_message_id IS NOT NULL;
CREATE UNIQUE INDEX idx_session_inbox_delivery
    ON session_inbox(session_id, delivery_key) WHERE delivery_key IS NOT NULL;
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
