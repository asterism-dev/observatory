-- +goose Up
CREATE TABLE catalogs (
  service text PRIMARY KEY,
  digest text NOT NULL,
  payload bytea NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE catalogs;
