-- +goose Up
ALTER TABLE vms ADD COLUMN image_ref text, ADD COLUMN image_digest text;

-- +goose Down
ALTER TABLE vms DROP COLUMN image_ref, DROP COLUMN image_digest;
