-- +goose Up
ALTER TABLE vms ADD COLUMN kind text NOT NULL DEFAULT 'vm' CHECK (kind IN ('vm', 'builder'));
ALTER TABLE image_builds ADD COLUMN secret_hash bytea UNIQUE, ADD COLUMN live_log_bytes bigint NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE image_builds DROP COLUMN secret_hash, DROP COLUMN live_log_bytes;
ALTER TABLE vms DROP COLUMN kind;
