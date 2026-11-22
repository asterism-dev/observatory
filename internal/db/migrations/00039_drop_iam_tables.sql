-- +goose Up
DROP TABLE role_bindings, role_parents, role_rules, roles, team_members, teams, invites, sessions, ssh_keys, memberships, orgs, users;
DELETE FROM hub_secrets WHERE name = 'iam-signing-key';

-- +goose Down
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
  role text NOT NULL CHECK (role IN ('owner', 'admin', 'member')),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (org_id, user_id)
);

CREATE TABLE sessions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id),
  kind text NOT NULL CHECK (kind IN ('browser', 'cli')),
  token_hash bytea NOT NULL UNIQUE,
  user_agent text NOT NULL DEFAULT '',
  ip text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  last_used_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  revoked_at timestamptz
);

CREATE TABLE invites (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES orgs(id),
  token_hash bytea NOT NULL UNIQUE,
  role text NOT NULL CHECK (role IN ('owner', 'admin', 'member')),
  created_by uuid NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  used_at timestamptz,
  used_by uuid REFERENCES users(id),
  revoked_at timestamptz
);

CREATE TABLE teams (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES orgs(id),
  name text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (org_id, name)
);

CREATE TABLE team_members (
  team_id uuid NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
  user_id uuid NOT NULL REFERENCES users(id),
  admin boolean NOT NULL DEFAULT false,
  PRIMARY KEY (team_id, user_id)
);

CREATE TABLE roles (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES orgs(id),
  name text NOT NULL,
  description text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (org_id, name)
);

CREATE TABLE role_rules (
  role_id uuid NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
  position integer NOT NULL,
  effect text NOT NULL CHECK (effect IN ('allow', 'deny')),
  object text NOT NULL,
  action text NOT NULL,
  PRIMARY KEY (role_id, position)
);

CREATE TABLE role_parents (
  role_id uuid NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
  parent_builtin text CHECK (parent_builtin IN ('member', 'admin')),
  parent_role uuid REFERENCES roles(id) ON DELETE CASCADE,
  CHECK ((parent_builtin IS NULL) <> (parent_role IS NULL))
);
CREATE UNIQUE INDEX role_parents_unique ON role_parents (role_id, coalesce(parent_builtin, parent_role::text));

CREATE TABLE role_bindings (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES orgs(id),
  subject text NOT NULL,
  role_id uuid NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (org_id, subject, role_id)
);

CREATE TABLE ssh_keys (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name text NOT NULL,
  public_key text NOT NULL,
  fingerprint text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, fingerprint)
);
