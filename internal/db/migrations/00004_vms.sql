-- +goose Up
CREATE TABLE nodes (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), org_id uuid NOT NULL REFERENCES orgs(id),
  name text NOT NULL, enroll_hash bytea, enroll_expires timestamptz, credential_hash bytea, enrolled_at timestamptz,
  cpus int, memory_mb int, machine jsonb, version text, last_seen timestamptz,
  created_by uuid REFERENCES users(id), created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (org_id, name));
CREATE INDEX nodes_enroll_hash ON nodes (enroll_hash) WHERE enroll_hash IS NOT NULL;
CREATE TABLE ssh_keys (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name text NOT NULL, public_key text NOT NULL, fingerprint text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, fingerprint));
CREATE TABLE vms (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), org_id uuid NOT NULL REFERENCES orgs(id),
  node_id uuid NOT NULL REFERENCES nodes(id), user_id uuid REFERENCES users(id), team_id uuid REFERENCES teams(id),
  name text NOT NULL, cpus int NOT NULL, memory_mb int NOT NULL, disk_gb int NOT NULL,
  power text NOT NULL DEFAULT 'running' CHECK (power IN ('running','stopped')), stop_mode text,
  generation bigint NOT NULL DEFAULT 0, provisioned boolean NOT NULL DEFAULT false,
  state text NOT NULL DEFAULT 'pending', error text NOT NULL DEFAULT '', ip text NOT NULL DEFAULT '',
  created_by uuid REFERENCES users(id), created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (org_id, name),
  CHECK ((user_id IS NULL) <> (team_id IS NULL)));
CREATE INDEX vms_node ON vms (node_id);

-- +goose Down
DROP TABLE vms;
DROP TABLE ssh_keys;
DROP TABLE nodes;
