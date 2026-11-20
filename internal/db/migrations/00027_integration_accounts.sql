-- +goose Up
CREATE TABLE integration_accounts (
    id uuid PRIMARY KEY,
    integration_id uuid NOT NULL REFERENCES integrations (id) ON DELETE CASCADE,
    org_id uuid NOT NULL REFERENCES orgs (id),
    position int NOT NULL,
    active boolean NOT NULL DEFAULT false,
    activated_manually boolean NOT NULL DEFAULT false,
    account text NOT NULL DEFAULT '',
    source text NOT NULL CHECK (source IN ('setup-token', 'oauth')),
    nonce bytea NOT NULL,
    ciphertext bytea NOT NULL,
    refresh_nonce bytea,
    refresh_ciphertext bytea,
    access_expires_at timestamptz,
    status text NOT NULL DEFAULT 'ok' CHECK (status IN ('ok', 'expired')),
    last_error text NOT NULL DEFAULT '',
    five_hour_pct double precision,
    five_hour_resets_at timestamptz,
    week_pct double precision,
    week_resets_at timestamptz,
    usage_source text NOT NULL DEFAULT '' CHECK (usage_source IN ('', 'poll', 'proxy')),
    usage_at timestamptz,
    limited_until timestamptz,
    created_by uuid NOT NULL REFERENCES users (id),
    updated_by uuid NOT NULL REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (integration_id, position) DEFERRABLE INITIALLY DEFERRED,
    CHECK ((source = 'oauth') = (refresh_ciphertext IS NOT NULL AND refresh_nonce IS NOT NULL))
);
CREATE UNIQUE INDEX integration_accounts_one_active ON integration_accounts (integration_id) WHERE active;
CREATE UNIQUE INDEX integration_accounts_unique_email ON integration_accounts (integration_id, account) WHERE account <> '';

-- The first account reuses its integration's id, so the sealed bytes (AAD integration:<org>:<id>:<field>) still open.
INSERT INTO integration_accounts (id, integration_id, org_id, position, active, account, source, nonce, ciphertext, refresh_nonce,
    refresh_ciphertext, access_expires_at, status, last_error, created_by, updated_by, created_at, updated_at)
SELECT id, id, org_id, 0, true, account, source, nonce, ciphertext, refresh_nonce,
    refresh_ciphertext, access_expires_at, status, last_error, created_by, updated_by, created_at, updated_at
FROM integrations WHERE kind = 'claude';

ALTER TABLE integrations DROP CONSTRAINT integrations_check1;
ALTER TABLE integrations DROP CONSTRAINT integrations_refresh_sealed;
ALTER TABLE integrations ALTER COLUMN nonce DROP NOT NULL;
ALTER TABLE integrations ALTER COLUMN ciphertext DROP NOT NULL;
ALTER TABLE integrations ALTER COLUMN source DROP NOT NULL;
UPDATE integrations SET source = NULL, account = '', nonce = NULL, ciphertext = NULL, refresh_nonce = NULL, refresh_ciphertext = NULL,
    access_expires_at = NULL, status = 'ok', last_error = ''
WHERE kind = 'claude';
ALTER TABLE integrations ADD CONSTRAINT integrations_token_place CHECK ((kind = 'claude') = (ciphertext IS NULL));
ALTER TABLE integrations ADD COLUMN last_switch_at timestamptz;
ALTER TABLE integrations ADD COLUMN last_switch_from uuid;
ALTER TABLE integrations ADD COLUMN last_switch_to uuid;

-- +goose Down

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM integration_accounts a WHERE a.id <> a.integration_id
            OR EXISTS (SELECT 1 FROM integration_accounts b WHERE b.integration_id = a.integration_id AND b.id <> a.id)) THEN
        RAISE EXCEPTION 'a Claude integration has more than one account or an account added later; remove them before migrating down';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE integrations DROP COLUMN last_switch_at;
ALTER TABLE integrations DROP COLUMN last_switch_from;
ALTER TABLE integrations DROP COLUMN last_switch_to;
ALTER TABLE integrations DROP CONSTRAINT integrations_token_place;
UPDATE integrations i SET source = a.source, account = a.account, nonce = a.nonce, ciphertext = a.ciphertext,
    refresh_nonce = a.refresh_nonce, refresh_ciphertext = a.refresh_ciphertext, access_expires_at = a.access_expires_at,
    status = a.status, last_error = a.last_error
FROM integration_accounts a WHERE a.integration_id = i.id;
ALTER TABLE integrations ALTER COLUMN nonce SET NOT NULL;
ALTER TABLE integrations ALTER COLUMN ciphertext SET NOT NULL;
ALTER TABLE integrations ALTER COLUMN source SET NOT NULL;
ALTER TABLE integrations ADD CONSTRAINT integrations_check1 CHECK ((source = 'oauth') = (refresh_ciphertext IS NOT NULL));
ALTER TABLE integrations ADD CONSTRAINT integrations_refresh_sealed
    CHECK ((source = 'oauth') = (refresh_ciphertext IS NOT NULL AND refresh_nonce IS NOT NULL));
DROP TABLE integration_accounts;
