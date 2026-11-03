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
// but returns it from the API, which is how a later run finds the comment.
const commentMarker = "<!-- orch:pr-review -->"

// EventRefresh edits orch's earlier comment on a PR, and adds none if there is
// none: the Poster returns ErrNoComment.
const EventRefresh = "REFRESH"

// ErrNoComment is returned for an EventRefresh on a PR orch has not commented on.
var ErrNoComment = errors.New("no earlier comment to update")

// Poster submits a review on a PR, pinned to commit, so it can never land on
// commits pushed after the verdict. event is APPROVE, COMMENT or EventRefresh.
// A comment replaces orch's earlier one on the PR rather than adding another.
type Poster func(ctx context.Context, repo string, number int, event, commit, body string) error

// GHPost submits the review through gh api. The body goes on stdin as JSON,
// never on the command line.
func GHPost(ctx context.Context, repo string, number int, event, commit, body string) error {
	switch event {
	case "APPROVE":
		return ghSubmit(ctx, repo, number, event, commit, body)
	case "COMMENT":
		err := ghEditComment(ctx, repo, number, commit, body)
		if errors.Is(err, ErrNoComment) {
			return ghSubmit(ctx, repo, number, event, commit, markedComment(commit, body))
		}
		return err
	case EventRefresh:
		return ghEditComment(ctx, repo, number, commit, body)
	}
	return fmt.Errorf("unsupported review event %q", event)
}

// markedComment is the text of orch's comment. An edited review keeps the
// commit it was first pinned to on GitHub, so the text names the reviewed one.
func markedComment(commit, body string) string {
	if strings.TrimSpace(body) == "" {
		body = "The latest review found nothing further to raise."
	}
	return fmt.Sprintf("%s\n%s\n\n<sub>Reviewed at %s. This comment is updated when a later review changes.</sub>",
		commentMarker, body, commit)
}

func ghSubmit(ctx context.Context, repo string, number int, event, commit, body string) error {
	payload, err := json.Marshal(map[string]string{"commit_id": commit, "event": event, "body": body})
	if err != nil {
		return err
	}
	_, err = ghAPI(ctx, payload, "--method", "POST", fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, number), "--input", "-")
	return err
}

// ghEditComment replaces orch's earlier comment on the PR. A failure to look
// for one is an error, not ErrNoComment, so it cannot lead to a duplicate.
func ghEditComment(ctx context.Context, repo string, number int, commit, body string) error {
	id, err := ghFindComment(ctx, repo, number)
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
	_, err = ghAPI(ctx, payload, "--method", "PUT", fmt.Sprintf("repos/%s/pulls/%d/reviews/%d", repo, number, id), "--input", "-")
	return err
}

// ghFindComment returns the newest of the user's reviews on the PR that orch
// posted, or 0. Others can write the marker, so only the user's own count.
func ghFindComment(ctx context.Context, repo string, number int) (int64, error) {
	login, err := ghAPI(ctx, nil, "user", "--jq", ".login")
	if err != nil {
		return 0, err
	}
	out, err := ghAPI(ctx, nil, "--paginate", fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, number),
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

func ghAPI(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", append([]string{"api"}, args...)...)
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
