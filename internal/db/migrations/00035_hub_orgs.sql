-- +goose Up
CREATE TABLE hub_orgs (
  org_id uuid PRIMARY KEY,
  mgmt_slice cidr UNIQUE,
  created_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO hub_orgs (org_id, mgmt_slice) SELECT id, mgmt_slice FROM orgs WHERE mgmt_slice IS NOT NULL;
ALTER TABLE orgs DROP COLUMN mgmt_slice;

-- +goose Down
ALTER TABLE orgs ADD COLUMN mgmt_slice cidr UNIQUE;
UPDATE orgs o SET mgmt_slice = h.mgmt_slice FROM hub_orgs h WHERE h.org_id = o.id;
DROP TABLE hub_orgs;
