-- +goose Up
ALTER TABLE integrations ADD COLUMN quota_project text NOT NULL DEFAULT '';
ALTER TABLE integrations DROP CONSTRAINT integrations_kind_check;
ALTER TABLE integrations ADD CONSTRAINT integrations_kind_check CHECK (kind IN ('github', 'claude', 'linear', 'gcloud'));
ALTER TABLE integrations DROP CONSTRAINT integrations_source_check;
ALTER TABLE integrations ADD CONSTRAINT integrations_source_check CHECK (source IN ('pat', 'setup-token', 'oauth', 'api-key', 'adc'));
ALTER TABLE integrations ADD CONSTRAINT integrations_check1 CHECK ((source IN ('oauth', 'adc')) = (refresh_ciphertext IS NOT NULL));
ALTER TABLE integrations ADD CONSTRAINT integrations_refresh_sealed
    CHECK ((source IN ('oauth', 'adc')) = (refresh_ciphertext IS NOT NULL AND refresh_nonce IS NOT NULL));

-- +goose Down
ALTER TABLE integrations DROP CONSTRAINT integrations_check1;
ALTER TABLE integrations DROP CONSTRAINT integrations_refresh_sealed;
ALTER TABLE integrations DROP CONSTRAINT integrations_source_check;
ALTER TABLE integrations ADD CONSTRAINT integrations_source_check CHECK (source IN ('pat', 'setup-token', 'oauth', 'api-key'));
ALTER TABLE integrations DROP CONSTRAINT integrations_kind_check;
ALTER TABLE integrations ADD CONSTRAINT integrations_kind_check CHECK (kind IN ('github', 'claude', 'linear'));
ALTER TABLE integrations DROP COLUMN quota_project;
