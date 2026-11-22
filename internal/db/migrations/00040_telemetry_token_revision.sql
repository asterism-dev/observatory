-- +goose Up
ALTER TABLE telemetry_tokens ADD COLUMN authorized_revision bigint NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE telemetry_tokens DROP COLUMN authorized_revision;
