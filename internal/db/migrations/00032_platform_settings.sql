-- +goose Up
CREATE TABLE platform_settings (
    id boolean PRIMARY KEY DEFAULT true CHECK (id),
    guest_mtu int NOT NULL DEFAULT 1300 CHECK (guest_mtu BETWEEN 1000 AND 1400)
);
INSERT INTO platform_settings DEFAULT VALUES;

-- +goose Down
DROP TABLE platform_settings;
