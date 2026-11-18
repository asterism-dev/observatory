-- +goose Up
ALTER TABLE vms ADD COLUMN auto_suspend_minutes int NOT NULL DEFAULT 0 CHECK (auto_suspend_minutes = 0 OR auto_suspend_minutes BETWEEN 5 AND 1440);
ALTER TABLE vms ADD COLUMN wake_seq bigint NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE vms DROP COLUMN wake_seq;
ALTER TABLE vms DROP COLUMN auto_suspend_minutes;
