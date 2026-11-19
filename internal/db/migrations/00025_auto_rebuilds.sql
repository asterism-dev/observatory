-- +goose Up
ALTER TABLE images
    ADD COLUMN auto_schedule text NOT NULL DEFAULT '' CHECK (auto_schedule IN ('', 'daily', 'weekly')),
    ADD COLUMN auto_on_base_change boolean NOT NULL DEFAULT false,
    ADD COLUMN auto_by uuid REFERENCES users (id) ON DELETE SET NULL,
    ADD COLUMN auto_revision integer,
    ADD COLUMN auto_error text NOT NULL DEFAULT '',
    ADD COLUMN auto_last_at timestamptz,
    ADD COLUMN auto_checked_at timestamptz;
ALTER TABLE image_versions ADD COLUMN base_digests jsonb NOT NULL DEFAULT '[]';
ALTER TABLE image_builds ADD COLUMN automatic boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE image_builds DROP COLUMN automatic;
ALTER TABLE image_versions DROP COLUMN base_digests;
ALTER TABLE images DROP COLUMN auto_schedule, DROP COLUMN auto_on_base_change, DROP COLUMN auto_by, DROP COLUMN auto_revision,
    DROP COLUMN auto_error, DROP COLUMN auto_last_at, DROP COLUMN auto_checked_at;
