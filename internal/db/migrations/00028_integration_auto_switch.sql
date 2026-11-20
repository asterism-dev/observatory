-- +goose Up
ALTER TABLE integrations ADD COLUMN auto_switch boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE integrations DROP COLUMN auto_switch;
