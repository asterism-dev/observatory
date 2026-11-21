-- +goose Up
CREATE TABLE outbox (
    id uuid PRIMARY KEY,
    subject text NOT NULL,
    payload bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    sent_at timestamptz
);
CREATE INDEX outbox_unsent ON outbox (id) WHERE sent_at IS NULL;
CREATE INDEX outbox_sent_at ON outbox (sent_at) WHERE sent_at IS NOT NULL;

-- +goose Down
DROP TABLE outbox;
