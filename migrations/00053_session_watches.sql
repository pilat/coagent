-- +goose Up

CREATE TABLE IF NOT EXISTS session_watches (
    watcher_id INTEGER NOT NULL REFERENCES sessions(id),
    target_id INTEGER NOT NULL REFERENCES sessions(id),
    read_through INTEGER NOT NULL DEFAULT 0 CHECK (read_through >= 0),
    PRIMARY KEY (watcher_id, target_id)
);
