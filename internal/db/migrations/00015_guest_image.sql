-- +goose Up
ALTER TABLE nodes ADD COLUMN image jsonb;
ALTER TABLE vms ADD COLUMN image text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE vms DROP COLUMN image;
ALTER TABLE nodes DROP COLUMN image;
