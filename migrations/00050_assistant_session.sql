-- +goose Up

DELETE FROM assistant_search WHERE rowid % 4 = 1;
DROP TABLE assistant_tape;
DROP TABLE assistant_turns;
