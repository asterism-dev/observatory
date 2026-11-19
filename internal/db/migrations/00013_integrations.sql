-- +goose Up
CREATE TABLE integrations (
    id uuid PRIMARY KEY,
    org_id uuid NOT NULL REFERENCES orgs (id),
    kind text NOT NULL CHECK (kind IN ('github', 'claude', 'linear')),
    source text NOT NULL CHECK (source IN ('pat', 'setup-token', 'oauth', 'api-key')),
    scope text NOT NULL CHECK (scope IN ('org', 'team', 'user')),
    team_id uuid REFERENCES teams (id) ON DELETE CASCADE,
    user_id uuid REFERENCES users (id),
    mode text NOT NULL DEFAULT 'proxy' CHECK (mode IN ('proxy', 'vm')),
    account text NOT NULL DEFAULT '',
    scopes_info text NOT NULL DEFAULT '',
    provider_expires_at timestamptz,
    nonce bytea NOT NULL,
    ciphertext bytea NOT NULL,
    refresh_nonce bytea,
    refresh_ciphertext bytea,
    access_expires_at timestamptz,
    placeholder text NOT NULL UNIQUE,
    status text NOT NULL DEFAULT 'ok' CHECK (status IN ('ok', 'expired')),
    last_error text NOT NULL DEFAULT '',
    was_in_vm boolean NOT NULL DEFAULT false,
    created_by uuid NOT NULL REFERENCES users (id),
    updated_by uuid NOT NULL REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((scope = 'org' AND team_id IS NULL AND user_id IS NULL)
        OR (scope = 'team' AND team_id IS NOT NULL AND user_id IS NULL)
        OR (scope = 'user' AND user_id IS NOT NULL AND team_id IS NULL)),
    CHECK ((source = 'oauth') = (refresh_ciphertext IS NOT NULL))
);
CREATE UNIQUE INDEX integrations_unique ON integrations (org_id, scope, coalesce(team_id, user_id, '00000000-0000-0000-0000-000000000000'::uuid), kind);

-- +goose Down
DROP TABLE integrations;
