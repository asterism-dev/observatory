-- +goose Up
CREATE TABLE dashboards (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    scope text NOT NULL CHECK (scope IN ('org', 'team', 'user')),
    team_id uuid REFERENCES teams (id) ON DELETE CASCADE,
    user_id uuid REFERENCES users (id) ON DELETE CASCADE,
    name text NOT NULL,
    panels jsonb NOT NULL DEFAULT '[]',
    created_by uuid NOT NULL REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((scope = 'org' AND team_id IS NULL AND user_id IS NULL)
        OR (scope = 'team' AND team_id IS NOT NULL AND user_id IS NULL)
        OR (scope = 'user' AND user_id IS NOT NULL AND team_id IS NULL))
);
CREATE INDEX dashboards_org ON dashboards (org_id);

-- +goose Down
DROP TABLE dashboards;
