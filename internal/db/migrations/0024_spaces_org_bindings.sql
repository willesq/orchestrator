-- +goose Up
-- Each GitHub account the applied spaces policy binds, with the node ID it
-- had when the operator applied. A login can be deleted and registered again
-- by someone else; the node ID cannot, so a sync that sees another node ID
-- suspends the binding until the operator confirms at the next apply.
CREATE TABLE spaces_org_bindings (
    org          TEXT PRIMARY KEY, -- the login, lowercased
    login        TEXT NOT NULL,    -- the login as GitHub spells it
    node_id      TEXT NOT NULL,    -- the node ID confirmed at apply
    revision     INTEGER NOT NULL REFERENCES spaces_revisions(revision),
    bound_at     TEXT NOT NULL,    -- RFC 3339 UTC
    -- Set when a sync saw seen_node_id instead of node_id; new content from
    -- the org goes to quarantine until an apply confirms or clears it.
    suspended_at TEXT,
    seen_node_id TEXT
);

-- +goose Down
DROP TABLE spaces_org_bindings;
