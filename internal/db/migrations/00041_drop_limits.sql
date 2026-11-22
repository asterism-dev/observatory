-- +goose Up
DROP TABLE limits;

-- +goose Down
CREATE TABLE limits (org_id uuid NOT NULL, scope text NOT NULL,
  vcpu int CHECK (vcpu >= 0), ram_mb int CHECK (ram_mb >= 0), disk_gb int CHECK (disk_gb >= 0), vm_count int CHECK (vm_count >= 0),
  snapshot_count int CHECK (snapshot_count >= 0), image_storage_gb int CHECK (image_storage_gb >= 0),
  PRIMARY KEY (org_id, scope));
