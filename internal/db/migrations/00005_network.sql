-- +goose Up
ALTER TABLE orgs ADD COLUMN mgmt_slice cidr UNIQUE;
ALTER TABLE vms ADD COLUMN mgmt_ip inet UNIQUE;
ALTER TABLE nodes ADD COLUMN chassis_key text UNIQUE, ADD COLUMN underlay_ip inet UNIQUE;
CREATE TABLE devices (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), org_id uuid NOT NULL REFERENCES orgs(id),
  user_id uuid NOT NULL REFERENCES users(id), name text NOT NULL, public_key text UNIQUE NOT NULL,
  ip inet UNIQUE NOT NULL, last_handshake timestamptz, created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (org_id, user_id, name),
  FOREIGN KEY (org_id, user_id) REFERENCES memberships (org_id, user_id) ON DELETE CASCADE);
CREATE TABLE networks (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), org_id uuid NOT NULL REFERENCES orgs(id),
  name text NOT NULL, subnet cidr NOT NULL, created_by uuid REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (org_id, name),
  EXCLUDE USING gist (subnet inet_ops WITH &&));
CREATE TABLE vm_networks (vm_id uuid NOT NULL REFERENCES vms(id) ON DELETE CASCADE,
  network_id uuid NOT NULL REFERENCES networks(id), ip inet UNIQUE NOT NULL,
  PRIMARY KEY (vm_id, network_id));
CREATE TABLE hub_secrets (name text PRIMARY KEY, key_id text NOT NULL, nonce bytea NOT NULL, sealed bytea NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now());

-- +goose Down
DROP TABLE hub_secrets;
DROP TABLE vm_networks;
DROP TABLE networks;
DROP TABLE devices;
ALTER TABLE nodes DROP COLUMN underlay_ip, DROP COLUMN chassis_key;
ALTER TABLE vms DROP COLUMN mgmt_ip;
ALTER TABLE orgs DROP COLUMN mgmt_slice;
