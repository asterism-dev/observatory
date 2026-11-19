-- +goose Up
ALTER TABLE vms ADD COLUMN legacy_runtime boolean NOT NULL DEFAULT false;
UPDATE vms SET legacy_runtime = true WHERE provisioned;

-- +goose Down
ALTER TABLE vms DROP COLUMN legacy_runtime;
