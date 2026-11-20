-- +goose Up
CREATE TABLE telemetry_tokens (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    name text NOT NULL,
    token_hash bytea NOT NULL UNIQUE,
    created_by uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    revoked_at timestamptz
);
CREATE UNIQUE INDEX telemetry_tokens_active_name ON telemetry_tokens (org_id, name) WHERE revoked_at IS NULL;

-- +goose Down
DROP TABLE telemetry_tokens;
