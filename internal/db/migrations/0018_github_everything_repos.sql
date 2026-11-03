-- +goose Up
-- everything_repos lists repos watched for every open item in a scope otherwise
-- watched only for items involving the user.
ALTER TABLE github_scopes ADD COLUMN everything_repos TEXT NOT NULL DEFAULT ''; -- comma-separated repo names, without the owner

-- +goose Down
ALTER TABLE github_scopes DROP COLUMN everything_repos;
