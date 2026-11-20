-- +goose Up
ALTER TABLE vms ADD COLUMN delete_at timestamptz;
CREATE INDEX vms_delete_at ON vms (delete_at) WHERE delete_at IS NOT NULL;
CREATE TABLE vm_history (id uuid PRIMARY KEY, org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  user_id uuid REFERENCES users(id) ON DELETE SET NULL, team_id uuid REFERENCES teams(id) ON DELETE SET NULL,
  node_id uuid REFERENCES nodes(id) ON DELETE SET NULL,
  name text NOT NULL, cpus int NOT NULL, memory_mb int NOT NULL, disk_gb int NOT NULL, image_ref text NOT NULL DEFAULT '',
  created_by uuid REFERENCES users(id) ON DELETE SET NULL, created_at timestamptz NOT NULL,
  deleted_by uuid REFERENCES users(id) ON DELETE SET NULL, deleted_at timestamptz NOT NULL DEFAULT now(),
  reason text NOT NULL CHECK (reason IN ('user', 'expired')));
CREATE INDEX vm_history_org ON vm_history (org_id, deleted_at DESC);

-- +goose Down
DROP TABLE vm_history;
ALTER TABLE vms DROP COLUMN delete_at;
