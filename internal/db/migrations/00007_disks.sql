-- +goose Up
CREATE TABLE disks (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), org_id uuid NOT NULL REFERENCES orgs(id),
  user_id uuid REFERENCES users(id), team_id uuid REFERENCES teams(id) ON DELETE SET NULL,
  name text NOT NULL, size_gb int NOT NULL CHECK (size_gb BETWEEN 1 AND 4096), ready_gb int NOT NULL DEFAULT 0,
  copy_from uuid REFERENCES disks(id), created_by uuid REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now(), deleted_at timestamptz,
  -- soft-deleted rows outlive their team (SET NULL) until the storage server reports the file gone
  CHECK (deleted_at IS NOT NULL OR (user_id IS NULL) <> (team_id IS NULL)));
CREATE UNIQUE INDEX disks_live_name ON disks (org_id, name) WHERE deleted_at IS NULL;
CREATE TABLE vm_disks (vm_id uuid NOT NULL REFERENCES vms(id) ON DELETE CASCADE,
  disk_id uuid UNIQUE NOT NULL REFERENCES disks(id), mount_path text NOT NULL, position int NOT NULL,
  PRIMARY KEY (vm_id, disk_id), UNIQUE (vm_id, mount_path));
CREATE TABLE disk_exports (disk_id uuid PRIMARY KEY REFERENCES disks(id),
  node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  port int UNIQUE NOT NULL CHECK (port BETWEEN 10810 AND 11809),
  key_id text NOT NULL, nonce bytea NOT NULL, sealed bytea NOT NULL,
  releasing boolean NOT NULL DEFAULT false, release_seq bigint);

-- +goose Down
DROP TABLE disk_exports;
DROP TABLE vm_disks;
DROP TABLE disks;
