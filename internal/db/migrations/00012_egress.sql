-- +goose Up
ALTER TABLE env_vars ADD COLUMN hosts text[];
ALTER TABLE env_vars ADD COLUMN placeholder text UNIQUE;
ALTER TABLE env_vars ADD CONSTRAINT env_vars_hosts_secret CHECK (hosts IS NULL OR kind = 'secret');
ALTER TABLE env_vars ADD CONSTRAINT env_vars_hosts_placeholder CHECK (hosts IS NULL OR placeholder IS NOT NULL);

CREATE TABLE vm_egress_cas (
    id uuid PRIMARY KEY,
    org_id uuid NOT NULL REFERENCES orgs (id),
    vm_id uuid NOT NULL REFERENCES vms (id) ON DELETE CASCADE,
    hosts text[] NOT NULL,
    cert_pem text NOT NULL,
    nonce bytea NOT NULL,
    key_ciphertext bytea NOT NULL,
    not_before timestamptz NOT NULL,
    not_after timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX vm_egress_cas_vm_idx ON vm_egress_cas (vm_id);

-- +goose Down
DROP TABLE vm_egress_cas;
ALTER TABLE env_vars DROP COLUMN placeholder;
ALTER TABLE env_vars DROP COLUMN hosts;
