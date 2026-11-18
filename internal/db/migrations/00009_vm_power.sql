-- +goose Up
ALTER TABLE vms DROP CONSTRAINT vms_power_check;
ALTER TABLE vms ADD CONSTRAINT vms_power_check CHECK (power IN ('running','stopped','paused','hibernated'));

-- +goose Down
UPDATE vms SET power = 'running' WHERE power IN ('paused','hibernated');
ALTER TABLE vms DROP CONSTRAINT vms_power_check;
ALTER TABLE vms ADD CONSTRAINT vms_power_check CHECK (power IN ('running','stopped'));
