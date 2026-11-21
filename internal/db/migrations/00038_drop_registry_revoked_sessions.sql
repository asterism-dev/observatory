-- +goose Up
DROP TABLE registry_revoked_sessions;

-- +goose Down
CREATE TABLE registry_revoked_sessions (
  session_id uuid PRIMARY KEY,
  expires_at timestamptz NOT NULL
);
