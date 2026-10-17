-- +goose Up

CREATE TABLE IF NOT EXISTS assistant_notes (
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    key TEXT NOT NULL,
    text TEXT NOT NULL,
    written_at INTEGER NOT NULL,
    PRIMARY KEY (session_id, key)
);

INSERT OR IGNORE INTO assistant_notes (session_id, key, text, written_at)
SELECT d.session_id, j.key, j.value, 0
FROM assistant_desk d, json_each(d.diary) j
WHERE j.type = 'text' AND (
    j.key GLOB 'user.*' OR j.key GLOB 'rule.*' OR j.key GLOB 'open.*'
);

INSERT OR IGNORE INTO assistant_notes (session_id, key, text, written_at)
SELECT d.session_id,
    CASE j.key WHEN 'integration.google-docs' THEN 'open.google-docs' ELSE 'rule.git' END,
    j.value, 0
FROM assistant_desk d, json_each(d.diary) j
WHERE j.type = 'text' AND j.key IN ('integration.google-docs', 'git.policy');

INSERT OR IGNORE INTO assistant_notes (session_id, key, text, written_at)
SELECT d.session_id, 'wait.' || json_extract(j.value, '$.on'), json_extract(j.value, '$.text'), 0
FROM assistant_desk d, json_each(d.stickers) j
WHERE json_type(j.value, '$.on') = 'text'
    AND json_extract(j.value, '$.on') GLOB '[0-9]*'
    AND json_extract(j.value, '$.on') NOT GLOB '*[^0-9]*'
    AND json_type(j.value, '$.text') = 'text';
