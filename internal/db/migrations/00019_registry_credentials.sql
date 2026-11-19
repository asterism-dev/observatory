-- +goose Up
CREATE TABLE registry_credentials (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id uuid NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    secret_hash bytea NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);

-- +goose Down
DROP TABLE registry_credentials;
