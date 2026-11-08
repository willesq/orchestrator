package reviews

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/willesq/orchestrator/internal/github"
)

// commentMarker starts every comment orch posts. GitHub does not render it,
// but returns it from the API, which is how a later run finds the comment when
// the user chose to update it in place.
const commentMarker = "<!-- orch:pr-review -->"

const (
	// EventUpdate edits orch's earlier comment on a PR, or adds one if there is none.
	EventUpdate = "UPDATE"
	// EventRefresh edits orch's earlier comment on a PR, and adds none if there
	// is none: the Poster returns ErrNoComment.
	EventRefresh = "REFRESH"
)

// ErrNoComment is returned for an EventRefresh on a PR orch has not commented on.
var ErrNoComment = errors.New("no earlier comment to update")

// Poster submits a review on a PR, pinned to commit, so it can never land on
// commits pushed after the verdict. event is APPROVE, COMMENT, EventUpdate or
// EventRefresh.
type Poster func(ctx context.Context, repo string, number int, event, commit, body string) error

// GHPost submits the review through gh api with the default login. The body
// goes on stdin as JSON, never on the command line.
func GHPost(ctx context.Context, repo string, number int, event, commit, body string) error {
	return ghPost(ctx, nil, repo, number, event, commit, body)
}

// NewGHPoster is GHPost run with the gh environment envFor returns for the
// repo's owner, so a review on a space's org is posted as that space's
// account; nil from envFor is the default login, an error refuses the post.
func NewGHPoster(envFor func(ctx context.Context, owner string) ([]string, error)) Poster {
	return func(ctx context.Context, repo string, number int, event, commit, body string) error {
		owner, _, _ := strings.Cut(repo, "/")
		env, err := envFor(ctx, owner)
		if err != nil {
			return err
		}
		return ghPost(ctx, env, repo, number, event, commit, body)
	}
}

func ghPost(ctx context.Context, env []string, repo string, number int, event, commit, body string) error {
	switch event {
	case "APPROVE":
		return ghSubmit(ctx, env, repo, number, event, commit, body)
	case "COMMENT":
		return ghSubmit(ctx, env, repo, number, event, commit, markedComment(commit, body))
	case EventUpdate:
		err := ghEditComment(ctx, env, repo, number, commit, body)
		if errors.Is(err, ErrNoComment) {
			return ghSubmit(ctx, env, repo, number, "COMMENT", commit, markedComment(commit, body))
		}
		return err
	case EventRefresh:
		return ghEditComment(ctx, env, repo, number, commit, body)
	}
	return fmt.Errorf("unsupported review event %q", event)
}

// markedComment is the text of orch's comment. An edited review keeps the
// commit it was first pinned to on GitHub, so the text names the reviewed one.
func markedComment(commit, body string) string {
	if strings.TrimSpace(body) == "" {
		body = "The latest review found nothing further to raise."
	}
	return fmt.Sprintf("%s\n%s\n\n<sub>Reviewed at %s.</sub>",
		commentMarker, body, commit)
}

func ghSubmit(ctx context.Context, env []string, repo string, number int, event, commit, body string) error {
	payload, err := json.Marshal(map[string]string{"commit_id": commit, "event": event, "body": body})
	if err != nil {
		return err
	}
	_, err = ghAPI(ctx, env, payload, "--method", "POST", fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, number), "--input", "-")
	return err
}

// ghEditComment replaces orch's earlier comment on the PR. A failure to look
// for one is an error, not ErrNoComment, so it cannot lead to a duplicate.
func ghEditComment(ctx context.Context, env []string, repo string, number int, commit, body string) error {
	id, err := ghFindComment(ctx, env, repo, number)
	if err != nil {
		return err
	}
	if id == 0 {
		return ErrNoComment
	}
	payload, err := json.Marshal(map[string]string{"body": markedComment(commit, body)})
	if err != nil {
		return err
	}
	_, err = ghAPI(ctx, env, payload, "--method", "PUT", fmt.Sprintf("repos/%s/pulls/%d/reviews/%d", repo, number, id), "--input", "-")
	return err
}

// ghFindComment returns the newest of the user's reviews on the PR that orch
// posted, or 0. Others can write the marker, so only the user's own count.
func ghFindComment(ctx context.Context, env []string, repo string, number int) (int64, error) {
	login, err := ghAPI(ctx, env, nil, "user", "--jq", ".login")
	if err != nil {
		return 0, err
	}
	out, err := ghAPI(ctx, env, nil, "--paginate", fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, number),
		"--jq", ".[] | {id, state, body, login: .user.login}")
	if err != nil {
		return 0, err
	}
	return newestComment(out, strings.TrimSpace(string(login)))
}

// newestComment picks from reviews, one JSON object each, oldest first.
func newestComment(reviews []byte, login string) (int64, error) {
	var id int64
	dec := json.NewDecoder(bytes.NewReader(reviews))
	for {
		var r struct {
			ID                 int64
			State, Body, Login string
		}
		if err := dec.Decode(&r); errors.Is(err, io.EOF) {
			return id, nil
		} else if err != nil {
			return 0, fmt.Errorf("read the PR's reviews: %w", err)
		}
		if r.State == "COMMENTED" && strings.EqualFold(r.Login, login) && strings.HasPrefix(r.Body, commentMarker) {
			id = r.ID
		}
	}
}

// ghAPI runs gh api with env as its environment; nil is this process's.
func ghAPI(ctx context.Context, env []string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", append([]string{"api"}, args...)...)
	cmd.Env = env
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && stderr.Len() > 0 {
			return nil, fmt.Errorf("gh api: %s", github.Printable(stderr.String(), 512))
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}
