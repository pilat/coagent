-- +goose Up
-- Pre-fix summary rows rolled up the cost of originals that still live in the DB, so
-- the lifetime tree-sum double-counts them. Zero the rollup; each original counts once.
UPDATE messages SET cost_usd = 0 WHERE role = 'user' AND content LIKE '[CONTEXT SUMMARY%';
