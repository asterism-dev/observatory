-- +goose Up
-- +goose StatementBegin
DO $$
DECLARE fk record;
BEGIN
  -- role_rules and role_parents are part of IAM's role graph, so their cascades from roles stay.
  FOR fk IN
    SELECT conrelid::regclass AS tbl, conname FROM pg_constraint
    WHERE contype = 'f'
      AND confrelid = ANY ('{users,orgs,memberships,sessions,teams,team_members,roles,role_bindings,invites,ssh_keys}'::regclass[])
      AND NOT conrelid = ANY ('{users,orgs,memberships,sessions,teams,team_members,roles,role_bindings,invites,ssh_keys,role_rules,role_parents}'::regclass[])
  LOOP
    EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I', fk.tbl, fk.conname);
  END LOOP;
END $$;
-- +goose StatementEnd

ALTER TABLE registry_credentials ADD COLUMN user_id uuid, ADD COLUMN session_expires_at timestamptz;
-- Validity no longer joins sessions, so credentials of revoked or expired sessions go now.
DELETE FROM registry_credentials c USING sessions s
  WHERE s.id = c.session_id AND (s.revoked_at IS NOT NULL OR s.expires_at <= now());
UPDATE registry_credentials c SET user_id = s.user_id, session_expires_at = s.expires_at FROM sessions s WHERE s.id = c.session_id;
ALTER TABLE registry_credentials ALTER COLUMN user_id SET NOT NULL, ALTER COLUMN session_expires_at SET NOT NULL;

-- +goose Down
ALTER TABLE registry_credentials DROP COLUMN user_id, DROP COLUMN session_expires_at;
-- The dropped foreign keys are not restored: rows written since may reference ids the iam tables lack.
