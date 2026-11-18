-- +goose Up
CREATE TABLE vm_snapshots (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), org_id uuid NOT NULL REFERENCES orgs(id),
  vm_id uuid NOT NULL REFERENCES vms(id) ON DELETE CASCADE, name text NOT NULL, memory boolean NOT NULL,
  state text NOT NULL DEFAULT 'creating' CHECK (state IN ('creating','ready','error')), error text NOT NULL DEFAULT '',
  size_bytes bigint NOT NULL DEFAULT 0, cpus int NOT NULL, memory_mb int NOT NULL, disk_gb int NOT NULL,
  networks jsonb NOT NULL, attachments jsonb NOT NULL, reported_at timestamptz,
  created_by uuid REFERENCES users(id), created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (vm_id, name));
-- source_disk_id has no FK: the source disk may be deleted while its copy lives on
CREATE TABLE snapshot_disks (id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  snapshot_id uuid REFERENCES vm_snapshots(id) ON DELETE SET NULL, source_disk_id uuid NOT NULL,
  disk_name text NOT NULL, mount_path text NOT NULL, position int NOT NULL, size_gb int NOT NULL,
  ready_gb int NOT NULL DEFAULT 0, deleted_at timestamptz);
CREATE INDEX snapshot_disks_snapshot ON snapshot_disks (snapshot_id);
ALTER TABLE disks ADD COLUMN copy_from_snapshot uuid REFERENCES snapshot_disks(id),
  ADD COLUMN restore_from uuid REFERENCES snapshot_disks(id), ADD COLUMN restore_token text;
ALTER TABLE vms ADD COLUMN restore_snapshot_id uuid REFERENCES vm_snapshots(id) ON DELETE SET NULL,
  ADD COLUMN restore_memory boolean NOT NULL DEFAULT false, ADD COLUMN restore_seq bigint NOT NULL DEFAULT 0,
  ADD COLUMN restore_state text NOT NULL DEFAULT '' CHECK (restore_state IN ('','node','storage','done')),
  ADD COLUMN fork_snapshot_id uuid, ADD COLUMN fork_source_vm uuid, ADD COLUMN fork_memory boolean NOT NULL DEFAULT false;
ALTER TABLE limits ADD COLUMN snapshot_count int CHECK (snapshot_count >= 0);

-- +goose Down
ALTER TABLE limits DROP COLUMN snapshot_count;
ALTER TABLE vms DROP COLUMN restore_snapshot_id, DROP COLUMN restore_memory, DROP COLUMN restore_seq,
  DROP COLUMN restore_state, DROP COLUMN fork_snapshot_id, DROP COLUMN fork_source_vm, DROP COLUMN fork_memory;
ALTER TABLE disks DROP COLUMN copy_from_snapshot, DROP COLUMN restore_from, DROP COLUMN restore_token;
DROP TABLE snapshot_disks;
DROP TABLE vm_snapshots;
