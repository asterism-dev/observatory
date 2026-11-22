-- +goose Up
CREATE TABLE orgs (
    -- Locked by every change that must see the org's limits and usage unchanged.
    org uuid PRIMARY KEY
);

-- scope is org, team:<uuid> or user:<uuid>; NULL is unlimited.
CREATE TABLE limits (
    org uuid NOT NULL,
    scope text NOT NULL,
    vcpu int CHECK (vcpu >= 0),
    ram_mb int CHECK (ram_mb >= 0),
    disk_gb int CHECK (disk_gb >= 0),
    vm_count int CHECK (vm_count >= 0),
    snapshot_count int CHECK (snapshot_count >= 0),
    image_storage_gb int CHECK (image_storage_gb >= 0),
    PRIMARY KEY (org, scope)
);

-- resource is vm/<id>, disk/<id> or snapshot/<id>; deleted rows are final tombstones (ids are never reused), keyed by org so a wrong-org release hides nothing.
CREATE TABLE allocations (
    org uuid NOT NULL,
    resource text NOT NULL,
    owner text NOT NULL,
    parent text,
    vcpu int NOT NULL DEFAULT 0 CHECK (vcpu >= 0),
    ram_mb int NOT NULL DEFAULT 0 CHECK (ram_mb >= 0),
    disk_gb int NOT NULL DEFAULT 0 CHECK (disk_gb >= 0),
    vm_count int NOT NULL DEFAULT 0 CHECK (vm_count >= 0),
    snapshot_count int NOT NULL DEFAULT 0 CHECK (snapshot_count >= 0),
    last_event_id uuid NOT NULL,
    deleted boolean NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org, resource)
);
CREATE INDEX allocations_live ON allocations (org, owner) WHERE NOT deleted;
CREATE INDEX allocations_parent ON allocations (org, parent) WHERE parent IS NOT NULL;
CREATE INDEX allocations_tombstones ON allocations (updated_at) WHERE deleted;

CREATE TABLE reservations (
    id uuid PRIMARY KEY,
    org uuid NOT NULL,
    owner text NOT NULL,
    vcpu int NOT NULL DEFAULT 0 CHECK (vcpu >= 0),
    ram_mb int NOT NULL DEFAULT 0 CHECK (ram_mb >= 0),
    disk_gb int NOT NULL DEFAULT 0 CHECK (disk_gb >= 0),
    vm_count int NOT NULL DEFAULT 0 CHECK (vm_count >= 0),
    snapshot_count int NOT NULL DEFAULT 0 CHECK (snapshot_count >= 0),
    expires_at timestamptz NOT NULL
);
CREATE INDEX reservations_org ON reservations (org, expires_at);

-- +goose Down
DROP TABLE reservations;
DROP TABLE allocations;
DROP TABLE limits;
DROP TABLE orgs;
