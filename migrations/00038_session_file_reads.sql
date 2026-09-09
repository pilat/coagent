-- +goose Up

CREATE TABLE session_file_reads (
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    path TEXT NOT NULL,
    mtime_unix_nano INTEGER NOT NULL,
    size INTEGER NOT NULL,
    hash TEXT NOT NULL,
    updated_at DATETIME NOT NULL,
    PRIMARY KEY (session_id, path)
);
