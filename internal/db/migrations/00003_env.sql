-- +goose Up
CREATE TABLE org_keys (org_id uuid PRIMARY KEY REFERENCES orgs(id), master_key_id text NOT NULL,
  nonce bytea NOT NULL, wrapped bytea NOT NULL, created_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE env_vars (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), org_id uuid NOT NULL REFERENCES orgs(id),
  kind text NOT NULL CHECK (kind IN ('env','secret')), scope text NOT NULL CHECK (scope IN ('org','team','user')),
  team_id uuid REFERENCES teams(id) ON DELETE CASCADE, user_id uuid REFERENCES users(id),
  name text NOT NULL, nonce bytea NOT NULL, ciphertext bytea NOT NULL,
  created_by uuid NOT NULL REFERENCES users(id), updated_by uuid NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
  CHECK ((scope = 'org' AND team_id IS NULL AND user_id IS NULL)
    OR (scope = 'team' AND team_id IS NOT NULL AND user_id IS NULL)
    OR (scope = 'user' AND user_id IS NOT NULL AND team_id IS NULL)));
CREATE UNIQUE INDEX env_vars_unique ON env_vars (org_id, scope, coalesce(team_id, user_id, '00000000-0000-0000-0000-000000000000'::uuid), name);

-- +goose Down
DROP TABLE env_vars;
DROP TABLE org_keys;
