-- +goose Up
CREATE TABLE images (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id uuid REFERENCES orgs (id),
    scope text NOT NULL CHECK (scope IN ('org', 'team', 'user')),
    team_id uuid REFERENCES teams (id) ON DELETE CASCADE,
    user_id uuid REFERENCES users (id),
    name text NOT NULL,
    description text NOT NULL DEFAULT '',
    recipe_revision int,
    platform boolean NOT NULL DEFAULT false,
    created_by uuid REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((scope = 'org' AND team_id IS NULL AND user_id IS NULL)
        OR (scope = 'team' AND team_id IS NOT NULL AND user_id IS NULL)
        OR (scope = 'user' AND user_id IS NOT NULL AND team_id IS NULL)),
    CHECK (platform = (org_id IS NULL)),
    CHECK (platform OR created_by IS NOT NULL)
);
CREATE UNIQUE INDEX images_name_unique ON images (org_id, scope, coalesce(team_id, user_id, '00000000-0000-0000-0000-000000000000'::uuid), name);

CREATE TABLE recipe_revisions (
    image_id uuid NOT NULL REFERENCES images (id) ON DELETE CASCADE,
    number int NOT NULL,
    refs jsonb NOT NULL DEFAULT '[]',
    created_by uuid NOT NULL REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (image_id, number)
);

CREATE TABLE recipe_files (
    image_id uuid NOT NULL,
    revision int NOT NULL,
    path text NOT NULL,
    mode bigint NOT NULL,
    content bytea NOT NULL,
    PRIMARY KEY (image_id, revision, path),
    FOREIGN KEY (image_id, revision) REFERENCES recipe_revisions (image_id, number) ON DELETE CASCADE
);

CREATE TABLE image_builds (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    image_id uuid NOT NULL REFERENCES images (id) ON DELETE CASCADE,
    revision int NOT NULL,
    state text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
    node_id uuid REFERENCES nodes (id) ON DELETE SET NULL,
    vm_id uuid REFERENCES vms (id) ON DELETE SET NULL,
    started_by uuid NOT NULL REFERENCES users (id),
    queued_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    finished_at timestamptz,
    digest text NOT NULL DEFAULT '',
    error text NOT NULL DEFAULT '',
    log bytea
);
CREATE UNIQUE INDEX image_builds_one_active ON image_builds (image_id) WHERE state IN ('queued', 'running');

CREATE TABLE image_build_log_chunks (
    id bigserial PRIMARY KEY,
    build_id uuid NOT NULL REFERENCES image_builds (id) ON DELETE CASCADE,
    chunk bytea NOT NULL
);
CREATE INDEX image_build_log_chunks_build ON image_build_log_chunks (build_id, id);

CREATE TABLE image_versions (
    image_id uuid NOT NULL REFERENCES images (id) ON DELETE CASCADE,
    digest text NOT NULL,
    source text NOT NULL CHECK (source IN ('build', 'push')),
    revision int,
    build_id uuid REFERENCES image_builds (id) ON DELETE SET NULL,
    size_bytes bigint NOT NULL DEFAULT 0,
    created_by uuid REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (image_id, digest)
);

CREATE TABLE image_tags (
    image_id uuid NOT NULL,
    tag text NOT NULL,
    digest text NOT NULL,
    PRIMARY KEY (image_id, tag),
    FOREIGN KEY (image_id, digest) REFERENCES image_versions (image_id, digest) ON DELETE CASCADE
);

CREATE TABLE image_version_layers (
    image_id uuid NOT NULL,
    version_digest text NOT NULL,
    digest text NOT NULL,
    size bigint NOT NULL,
    PRIMARY KEY (image_id, version_digest, digest),
    FOREIGN KEY (image_id, version_digest) REFERENCES image_versions (image_id, digest) ON DELETE CASCADE
);

CREATE TABLE image_defaults (
    org_id uuid NOT NULL REFERENCES orgs (id),
    scope text NOT NULL CHECK (scope IN ('org', 'team', 'user')),
    team_id uuid REFERENCES teams (id) ON DELETE CASCADE,
    user_id uuid REFERENCES users (id),
    image_id uuid NOT NULL REFERENCES images (id) ON DELETE CASCADE,
    tag text NOT NULL DEFAULT '',
    digest text NOT NULL DEFAULT '',
    CHECK ((scope = 'org' AND team_id IS NULL AND user_id IS NULL)
        OR (scope = 'team' AND team_id IS NOT NULL AND user_id IS NULL)
        OR (scope = 'user' AND user_id IS NOT NULL AND team_id IS NULL))
);
CREATE UNIQUE INDEX image_defaults_unique ON image_defaults (org_id, scope, coalesce(team_id, user_id, '00000000-0000-0000-0000-000000000000'::uuid));

ALTER TABLE limits ADD COLUMN image_storage_gb int CHECK (image_storage_gb >= 0);

-- +goose Down
ALTER TABLE limits DROP COLUMN image_storage_gb;
DROP TABLE image_defaults;
DROP TABLE image_version_layers;
DROP TABLE image_tags;
DROP TABLE image_versions;
DROP TABLE image_build_log_chunks;
DROP TABLE image_builds;
DROP TABLE recipe_files;
DROP TABLE recipe_revisions;
DROP TABLE images;
