-- +goose Up
CREATE TABLE accounts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id uuid REFERENCES orgs (id) ON DELETE CASCADE,
    team_id uuid UNIQUE REFERENCES teams (id) ON DELETE SET NULL,
    name text NOT NULL,
    platform boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, name),
    CHECK ((org_id IS NULL) = platform)
);
CREATE UNIQUE INDEX accounts_one_platform ON accounts (platform) WHERE platform;
INSERT INTO accounts (name, platform) VALUES ('platform', true);

-- +goose StatementBegin
CREATE FUNCTION account_name(org uuid, wanted text, self uuid) RETURNS text LANGUAGE sql STABLE AS $$
    -- A clash gets a suffix: old code must never fail on an account name.
    SELECT CASE
        WHEN NOT EXISTS (SELECT 1 FROM accounts WHERE org_id = org AND name = wanted AND id IS DISTINCT FROM self) THEN wanted
        WHEN NOT EXISTS (SELECT 1 FROM accounts WHERE org_id = org AND name = wanted || '-team' AND id IS DISTINCT FROM self) THEN wanted || '-team'
        ELSE wanted || '-' || left(gen_random_uuid()::text, 8)
    END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION account_for(org uuid, team uuid, is_platform boolean) RETURNS uuid LANGUAGE sql STABLE AS $$
    SELECT id FROM accounts WHERE
        CASE
            WHEN is_platform THEN platform
            WHEN team IS NOT NULL THEN team_id = team
            ELSE org_id = org AND team_id IS NULL AND name = 'default'
        END
    LIMIT 1
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION orgs_default_account() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO accounts (org_id, name) VALUES (NEW.id, 'default');
    RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER orgs_default_account AFTER INSERT ON orgs FOR EACH ROW EXECUTE FUNCTION orgs_default_account();

-- +goose StatementBegin
CREATE FUNCTION teams_account() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        INSERT INTO accounts (org_id, team_id, name) VALUES (NEW.org_id, NEW.id, account_name(NEW.org_id, NEW.name, NULL));
    ELSIF NEW.name IS DISTINCT FROM OLD.name THEN
        UPDATE accounts SET name = account_name(NEW.org_id, NEW.name, id) WHERE team_id = NEW.id;
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER teams_account AFTER INSERT OR UPDATE OF name ON teams FOR EACH ROW EXECUTE FUNCTION teams_account();

-- +goose StatementBegin
CREATE FUNCTION resource_account() RETURNS trigger LANGUAGE plpgsql AS $$
-- Keeps account_id right while the old model still writes ownership through org/team/user columns.
DECLARE
    j jsonb := to_jsonb(NEW);
BEGIN
    NEW.account_id := account_for((j->>'org_id')::uuid, (j->>'team_id')::uuid, coalesce((j->>'platform')::boolean, false));
    IF NEW.created_by IS NULL AND j->>'user_id' IS NOT NULL THEN
        NEW.created_by := (j->>'user_id')::uuid;
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

INSERT INTO accounts (org_id, name) SELECT id, 'default' FROM orgs;
-- +goose StatementBegin
DO $$
DECLARE
    t record;
BEGIN
    FOR t IN SELECT id, org_id, name FROM teams ORDER BY created_at, id LOOP
        INSERT INTO accounts (org_id, team_id, name) VALUES (t.org_id, t.id, account_name(t.org_id, t.name, NULL));
    END LOOP;
END
$$;
-- +goose StatementEnd

ALTER TABLE vms ADD COLUMN account_id uuid REFERENCES accounts (id) ON DELETE SET NULL, ADD COLUMN tags jsonb NOT NULL DEFAULT '{}';
ALTER TABLE disks ADD COLUMN account_id uuid REFERENCES accounts (id) ON DELETE SET NULL, ADD COLUMN tags jsonb NOT NULL DEFAULT '{}';
ALTER TABLE images ADD COLUMN account_id uuid REFERENCES accounts (id) ON DELETE SET NULL, ADD COLUMN tags jsonb NOT NULL DEFAULT '{}';
ALTER TABLE env_vars ADD COLUMN account_id uuid REFERENCES accounts (id) ON DELETE SET NULL, ADD COLUMN tags jsonb NOT NULL DEFAULT '{}';
ALTER TABLE integrations ADD COLUMN account_id uuid REFERENCES accounts (id) ON DELETE SET NULL, ADD COLUMN tags jsonb NOT NULL DEFAULT '{}';
ALTER TABLE dashboards ADD COLUMN account_id uuid REFERENCES accounts (id) ON DELETE SET NULL, ADD COLUMN tags jsonb NOT NULL DEFAULT '{}';
ALTER TABLE networks ADD COLUMN account_id uuid REFERENCES accounts (id) ON DELETE SET NULL, ADD COLUMN tags jsonb NOT NULL DEFAULT '{}';
ALTER TABLE nodes ADD COLUMN account_id uuid REFERENCES accounts (id) ON DELETE SET NULL, ADD COLUMN tags jsonb NOT NULL DEFAULT '{}';
ALTER TABLE telemetry_tokens ADD COLUMN account_id uuid REFERENCES accounts (id) ON DELETE SET NULL, ADD COLUMN tags jsonb NOT NULL DEFAULT '{}';

CREATE TRIGGER vms_account BEFORE INSERT OR UPDATE OF org_id, team_id, account_id ON vms FOR EACH ROW EXECUTE FUNCTION resource_account();
CREATE TRIGGER disks_account BEFORE INSERT OR UPDATE OF org_id, team_id, account_id ON disks FOR EACH ROW EXECUTE FUNCTION resource_account();
CREATE TRIGGER images_account BEFORE INSERT OR UPDATE OF org_id, team_id, platform, account_id ON images FOR EACH ROW EXECUTE FUNCTION resource_account();
CREATE TRIGGER env_vars_account BEFORE INSERT OR UPDATE OF org_id, team_id, account_id ON env_vars FOR EACH ROW EXECUTE FUNCTION resource_account();
CREATE TRIGGER integrations_account BEFORE INSERT OR UPDATE OF org_id, team_id, account_id ON integrations FOR EACH ROW EXECUTE FUNCTION resource_account();
CREATE TRIGGER dashboards_account BEFORE INSERT OR UPDATE OF org_id, team_id, account_id ON dashboards FOR EACH ROW EXECUTE FUNCTION resource_account();
CREATE TRIGGER networks_account BEFORE INSERT OR UPDATE OF org_id, account_id ON networks FOR EACH ROW EXECUTE FUNCTION resource_account();
CREATE TRIGGER nodes_account BEFORE INSERT OR UPDATE OF org_id, account_id ON nodes FOR EACH ROW EXECUTE FUNCTION resource_account();
CREATE TRIGGER telemetry_tokens_account BEFORE INSERT OR UPDATE OF org_id, account_id ON telemetry_tokens FOR EACH ROW EXECUTE FUNCTION resource_account();

UPDATE vms SET account_id = NULL;
UPDATE disks SET account_id = NULL;
UPDATE images SET account_id = NULL;
UPDATE env_vars SET account_id = NULL;
UPDATE integrations SET account_id = NULL;
UPDATE dashboards SET account_id = NULL;
UPDATE networks SET account_id = NULL;
UPDATE nodes SET account_id = NULL;
UPDATE telemetry_tokens SET account_id = NULL;

-- +goose Down
DROP TRIGGER telemetry_tokens_account ON telemetry_tokens;
DROP TRIGGER nodes_account ON nodes;
DROP TRIGGER networks_account ON networks;
DROP TRIGGER dashboards_account ON dashboards;
DROP TRIGGER integrations_account ON integrations;
DROP TRIGGER env_vars_account ON env_vars;
DROP TRIGGER images_account ON images;
DROP TRIGGER disks_account ON disks;
DROP TRIGGER vms_account ON vms;
ALTER TABLE telemetry_tokens DROP COLUMN account_id, DROP COLUMN tags;
ALTER TABLE nodes DROP COLUMN account_id, DROP COLUMN tags;
ALTER TABLE networks DROP COLUMN account_id, DROP COLUMN tags;
ALTER TABLE dashboards DROP COLUMN account_id, DROP COLUMN tags;
ALTER TABLE integrations DROP COLUMN account_id, DROP COLUMN tags;
ALTER TABLE env_vars DROP COLUMN account_id, DROP COLUMN tags;
ALTER TABLE images DROP COLUMN account_id, DROP COLUMN tags;
ALTER TABLE disks DROP COLUMN account_id, DROP COLUMN tags;
ALTER TABLE vms DROP COLUMN account_id, DROP COLUMN tags;
DROP FUNCTION resource_account();
DROP TRIGGER teams_account ON teams;
DROP FUNCTION teams_account();
DROP TRIGGER orgs_default_account ON orgs;
DROP FUNCTION orgs_default_account();
DROP FUNCTION account_for(uuid, uuid, boolean);
DROP FUNCTION account_name(uuid, text, uuid);
DROP TABLE accounts;
