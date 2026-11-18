-- +goose Up
ALTER TABLE integrations ADD CONSTRAINT integrations_refresh_sealed
    CHECK ((source = 'oauth') = (refresh_ciphertext IS NOT NULL AND refresh_nonce IS NOT NULL));

-- +goose Down
ALTER TABLE integrations DROP CONSTRAINT integrations_refresh_sealed;
