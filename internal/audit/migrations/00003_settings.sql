-- +goose Up
CREATE TABLE org_data_events (
    org uuid NOT NULL,
    service text NOT NULL,
    enabled boolean NOT NULL,
    PRIMARY KEY (org, service)
);

CREATE TABLE platform_settings (
    id boolean PRIMARY KEY DEFAULT true CHECK (id),
    retention_days integer NOT NULL DEFAULT 365 CHECK (retention_days BETWEEN 30 AND 3650)
);
INSERT INTO platform_settings DEFAULT VALUES;

-- +goose Down
DROP TABLE platform_settings;
DROP TABLE org_data_events;
