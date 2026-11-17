-- +goose Up
CREATE TABLE users (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  subject text NOT NULL UNIQUE,
  email text NOT NULL,
  name text NOT NULL,
  platform_admin boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE orgs (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL,
  personal_for uuid UNIQUE REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE memberships (
  org_id uuid NOT NULL REFERENCES orgs(id),
  user_id uuid NOT NULL REFERENCES users(id),
  role text NOT NULL CHECK (role IN ('owner','admin','member')),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (org_id, user_id)
);
CREATE TABLE sessions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id),
  kind text NOT NULL CHECK (kind IN ('browser','cli')),
  token_hash bytea NOT NULL UNIQUE,
  user_agent text NOT NULL DEFAULT '',
  ip text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  last_used_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  revoked_at timestamptz
);
CREATE TABLE audit_log (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  at timestamptz NOT NULL DEFAULT now(),
  actor_kind text NOT NULL CHECK (actor_kind IN ('user','vm','system')),
  actor_id text NOT NULL,
  org_id uuid,
  action text NOT NULL,
  resource text NOT NULL DEFAULT '',
  result text NOT NULL CHECK (result IN ('allowed','denied','failed')),
  ip text NOT NULL DEFAULT '',
  user_agent text NOT NULL DEFAULT '',
  details jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX audit_log_org_id ON audit_log (org_id, id DESC);
CREATE INDEX audit_log_actor ON audit_log (actor_id, id DESC);
-- +goose StatementBegin
CREATE FUNCTION audit_log_immutable() RETURNS trigger LANGUAGE plpgsql AS
  $$ BEGIN RAISE EXCEPTION 'audit_log is append-only'; END $$;
-- +goose StatementEnd
CREATE TRIGGER audit_log_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON audit_log
  FOR EACH STATEMENT EXECUTE FUNCTION audit_log_immutable();

-- +goose Down
DROP TRIGGER audit_log_immutable ON audit_log;
DROP FUNCTION audit_log_immutable();
DROP TABLE audit_log;
DROP TABLE sessions;
DROP TABLE memberships;
DROP TABLE orgs;
DROP TABLE users;
