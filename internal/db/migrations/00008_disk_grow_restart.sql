-- +goose Up
ALTER TABLE disks ADD COLUMN grow_restart boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE disks DROP COLUMN grow_restart;
