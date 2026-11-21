-- +goose Up
CREATE TABLE events (
    id uuid PRIMARY KEY,
    time timestamptz NOT NULL,
    type text NOT NULL,
    org uuid,
    actor_kind text NOT NULL,
    actor_id text NOT NULL,
    resource text NOT NULL,
    result text NOT NULL,
    client_ip text NOT NULL,
    user_agent text NOT NULL,
    request_id text NOT NULL,
    details jsonb NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX events_org_time ON events (org, time DESC, id DESC);
CREATE INDEX events_actor_time ON events (actor_id, time DESC);
CREATE INDEX events_time ON events (time DESC, id DESC);

-- +goose StatementBegin
CREATE FUNCTION events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' AND current_setting('observatory.retention', true) = 'on' THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'audit events are append-only (%)', TG_OP USING ERRCODE = 'insufficient_privilege';
END
$$;
-- +goose StatementEnd
-- Only the retention job may delete: it sets observatory.retention = 'on' for its transaction (SET LOCAL).
CREATE TRIGGER events_append_only BEFORE UPDATE OR DELETE ON events FOR EACH ROW EXECUTE FUNCTION events_append_only();
CREATE TRIGGER events_no_truncate BEFORE TRUNCATE ON events FOR EACH STATEMENT EXECUTE FUNCTION events_append_only();

-- +goose Down
DROP TABLE events;
DROP FUNCTION events_append_only();
