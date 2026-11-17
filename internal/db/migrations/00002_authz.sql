-- +goose Up
CREATE TABLE teams (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), org_id uuid NOT NULL REFERENCES orgs(id),
  name text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), UNIQUE (org_id, name));
CREATE TABLE team_members (team_id uuid NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
  user_id uuid NOT NULL REFERENCES users(id), admin boolean NOT NULL DEFAULT false, PRIMARY KEY (team_id, user_id));
CREATE TABLE roles (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), org_id uuid NOT NULL REFERENCES orgs(id),
  name text NOT NULL, description text NOT NULL DEFAULT '', created_at timestamptz NOT NULL DEFAULT now(), UNIQUE (org_id, name));
CREATE TABLE role_rules (role_id uuid NOT NULL REFERENCES roles(id) ON DELETE CASCADE, position int NOT NULL,
  effect text NOT NULL CHECK (effect IN ('allow','deny')), object text NOT NULL, action text NOT NULL, PRIMARY KEY (role_id, position));
CREATE TABLE role_parents (role_id uuid NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
  parent_builtin text CHECK (parent_builtin IN ('member','admin')), parent_role uuid REFERENCES roles(id) ON DELETE CASCADE,
  CHECK ((parent_builtin IS NULL) <> (parent_role IS NULL)));
CREATE UNIQUE INDEX role_parents_unique ON role_parents (role_id, coalesce(parent_builtin, parent_role::text));
CREATE TABLE role_bindings (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), org_id uuid NOT NULL REFERENCES orgs(id),
  subject text NOT NULL, role_id uuid NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(), UNIQUE (org_id, subject, role_id));
CREATE TABLE invites (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), org_id uuid NOT NULL REFERENCES orgs(id),
  token_hash bytea NOT NULL UNIQUE, role text NOT NULL CHECK (role IN ('owner','admin','member')),
  created_by uuid NOT NULL REFERENCES users(id), created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL, used_at timestamptz, used_by uuid REFERENCES users(id), revoked_at timestamptz);
CREATE TABLE limits (org_id uuid NOT NULL REFERENCES orgs(id), scope text NOT NULL,
  vcpu int CHECK (vcpu >= 0), ram_mb int CHECK (ram_mb >= 0), disk_gb int CHECK (disk_gb >= 0), vm_count int CHECK (vm_count >= 0),
  PRIMARY KEY (org_id, scope));

-- +goose Down
DROP TABLE limits;
DROP TABLE invites;
DROP TABLE role_bindings;
DROP TABLE role_parents;
DROP TABLE role_rules;
DROP TABLE roles;
DROP TABLE team_members;
DROP TABLE teams;
