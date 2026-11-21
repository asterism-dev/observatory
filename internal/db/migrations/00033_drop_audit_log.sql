-- +goose Up
DROP TABLE audit_log;
DROP FUNCTION audit_log_immutable();

-- +goose Down
CREATE TABLE audit_log (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  at timestamptz NOT NULL DEFAULT now(),
  actor_kind text NOT NULL CHECK (actor_kind IN ('user','vm','system','node')),
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
CREATE FUNCTION audit_log_immutable() RETURNS trigger LANGUAGE plpgsql AS
  $$ BEGIN RAISE EXCEPTION 'audit_log is append-only'; END $$;
CREATE TRIGGER audit_log_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON audit_log
  FOR EACH STATEMENT EXECUTE FUNCTION audit_log_immutable();
