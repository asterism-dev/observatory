-- +goose Up
ALTER TABLE nodes ADD COLUMN quarantined boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE nodes DROP COLUMN quarantined;
