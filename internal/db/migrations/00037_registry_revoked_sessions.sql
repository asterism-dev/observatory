-- +goose Up
CREATE TABLE registry_revoked_sessions (
  session_id uuid PRIMARY KEY,
  expires_at timestamptz NOT NULL
);

-- +goose Down
DROP TABLE registry_revoked_sessions;
