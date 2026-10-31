-- +goose Up
-- pr_reviews holds the PRs the user asked orch to review once they are ready.
-- Each row tracks one PR: what orch may post for it (mode), where its review
-- stands, and the last verdict with the head commit it covers.
CREATE TABLE IF NOT EXISTS pr_reviews (
    url         TEXT PRIMARY KEY,                  -- https://github.com/<owner>/<repo>/pull/<n>
    repo        TEXT NOT NULL,                     -- owner/name
    number      INTEGER NOT NULL,
    mode        TEXT NOT NULL DEFAULT 'verdict',   -- verdict | approve | comment | feedback | auto
    state       TEXT NOT NULL DEFAULT 'waiting',   -- waiting | held | reviewing | verdict | failed | closed | stopped
    head_sha    TEXT NOT NULL DEFAULT '',          -- the head the verdict covers
    pending_sha TEXT NOT NULL DEFAULT '',          -- the head under review
    task_id     TEXT NOT NULL DEFAULT '',          -- the review task, while one runs and after
    nonce       TEXT NOT NULL DEFAULT '',          -- marks the review task's own verdict block
    verdict     TEXT NOT NULL DEFAULT '',          -- approve | wait | changes
    reason      TEXT NOT NULL DEFAULT '',
    comment     TEXT NOT NULL DEFAULT '',          -- the drafted review comment
    instructions TEXT NOT NULL DEFAULT '',         -- the user's notes for this PR's reviews
    for_user    TEXT NOT NULL DEFAULT '',          -- the review's answer to the notes, for the user only
    report      TEXT NOT NULL DEFAULT '',          -- the full review, for the user only
    go_ahead    INTEGER NOT NULL DEFAULT 0,        -- 1: review the next ready commit even from an untrusted author
    posted      TEXT NOT NULL DEFAULT '',          -- approved | commented | failed, for head_sha
    note        TEXT NOT NULL DEFAULT '',          -- the last problem, or why nothing was posted
    created_at  INTEGER NOT NULL DEFAULT 0,        -- unix seconds
    updated_at  INTEGER NOT NULL DEFAULT 0,
    verdict_at  INTEGER NOT NULL DEFAULT 0
);

-- +goose Down
DROP TABLE IF EXISTS pr_reviews;
