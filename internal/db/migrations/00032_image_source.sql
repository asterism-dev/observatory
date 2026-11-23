-- +goose Up
ALTER TABLE images
    ADD COLUMN source_repo text NOT NULL DEFAULT '',
    ADD COLUMN source_ref text NOT NULL DEFAULT '',
    ADD COLUMN source_path text NOT NULL DEFAULT '';
ALTER TABLE recipe_revisions ADD COLUMN source text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE recipe_revisions DROP COLUMN source;
ALTER TABLE images
    DROP COLUMN source_path,
    DROP COLUMN source_ref,
    DROP COLUMN source_repo;
