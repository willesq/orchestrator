-- +goose Up
-- providers and models are the model registry: what each coding-agent CLI on this
-- machine can run, discovered from the CLI's own local cache (no credentials).
-- Discovery records models as 'discovered' and never enables one: a vendor
-- shipping a model must not redirect work on its own. Routing considers only
-- enabled models of enabled providers.
CREATE TABLE IF NOT EXISTS providers (
    name           TEXT PRIMARY KEY,           -- claude-code | codex | gemini
    state          TEXT NOT NULL CHECK (state IN ('enabled', 'disabled', 'unavailable')),
    -- prior_state is the state held when the CLI went missing, restored when it returns.
    prior_state    TEXT NOT NULL DEFAULT '' CHECK (prior_state IN ('', 'enabled', 'disabled')),
    state_reason   TEXT NOT NULL DEFAULT '',
    last_refreshed INTEGER NOT NULL DEFAULT 0  -- unix seconds
);

CREATE TABLE IF NOT EXISTS models (
    provider          TEXT NOT NULL,
    slug              TEXT NOT NULL,             -- alias or pinned id, as the CLI accepts it
    display_name      TEXT NOT NULL DEFAULT '',
    description       TEXT NOT NULL DEFAULT '',
    tier              TEXT NOT NULL DEFAULT '',  -- frontier | balanced | fast | '' (unknown)
    default_effort    TEXT NOT NULL DEFAULT '',  -- '' where the provider has no effort knob
    supported_efforts TEXT NOT NULL DEFAULT '',  -- comma-separated
    visibility        TEXT NOT NULL DEFAULT 'list' CHECK (visibility IN ('list', 'hide')),
    source            TEXT NOT NULL CHECK (source IN ('alias', 'cache')),
    last_verified     INTEGER NOT NULL DEFAULT 0,
    state             TEXT NOT NULL CHECK (state IN ('discovered', 'enabled', 'disabled')),
    PRIMARY KEY (provider, slug)
);

-- +goose Down
DROP TABLE models;
DROP TABLE providers;
