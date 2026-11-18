-- +goose Up
ALTER TABLE nodes ADD COLUMN image jsonb;
ALTER TABLE vms ADD COLUMN image text NOT NULL DEFAULT '';
-- Guest images need 10 GB; forks take their snapshot's disk instead.
UPDATE vms SET disk_gb = 10 WHERE NOT provisioned AND fork_snapshot_id IS NULL AND disk_gb < 10;

-- +goose Down
ALTER TABLE vms DROP COLUMN image;
ALTER TABLE nodes DROP COLUMN image;
