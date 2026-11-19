-- +goose Up
ALTER TABLE vms ADD COLUMN image_id uuid REFERENCES images (id) ON DELETE SET NULL, ADD COLUMN image_source text;

-- +goose Down
ALTER TABLE vms DROP COLUMN image_id, DROP COLUMN image_source;
