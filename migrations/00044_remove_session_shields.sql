-- +goose Up

ALTER TABLE sessions DROP COLUMN shields_up;
