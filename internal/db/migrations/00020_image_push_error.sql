-- +goose Up
ALTER TABLE images ADD COLUMN push_error text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE images DROP COLUMN push_error;
