-- +goose Up
ALTER TABLE image_versions ADD COLUMN platform_digest text;

-- +goose Down
ALTER TABLE image_versions DROP COLUMN platform_digest;
