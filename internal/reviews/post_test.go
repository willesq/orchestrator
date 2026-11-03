package reviews

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type ghCall struct {
	args  string
	input map[string]string
}

// recordingGH puts a gh on PATH that keeps every call and answers as me with
// reviews. A method listed in failing exits non-zero.
func recordingGH(t *testing.T, reviews string, failing ...string) func() []ghCall {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "reviews"), []byte(reviews), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
d="` + dir + `"
n=$(ls "$d" | grep -c '^args')
printf '%s ' "$@" > "$d/args$n"
cat > "$d/input$n"
for f in ` + strings.Join(failing, " ") + `; do
  case " $* " in *" $f "*) echo "HTTP 500: $f failed" >&2; exit 1;; esac
done
case "$2" in
  user) echo me ;;
  --paginate) cat "$d/reviews" ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []ghCall {
		var calls []ghCall
		for i := 0; ; i++ {
			args, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("args%d", i)))
			if err != nil {
				return calls
			}
			c := ghCall{args: strings.TrimSpace(string(args))}
			if input, _ := os.ReadFile(filepath.Join(dir, fmt.Sprintf("input%d", i))); len(input) > 0 {
				if err := json.Unmarshal(input, &c.input); err != nil {
					t.Fatal(err)
				}
			}
			calls = append(calls, c)
		}
	}
}

// writes keeps the calls that change something on GitHub.
func writes(calls []ghCall) []ghCall {
	var out []ghCall
	for _, c := range calls {
		if strings.Contains(c.args, "--method") {
			out = append(out, c)
		}
	}
	return out
}

func review(id int, state, login, body string) string {
	b, _ := json.Marshal(map[string]any{"id": id, "state": state, "login": login, "body": body})
	return string(b) + "\n"
}

const (
	createPath = "api --method POST repos/a/b/pulls/9/reviews --input -"
	mine       = commentMarker + "\nold"
)

func editPath(id int) string {
	return fmt.Sprintf("api --method PUT repos/a/b/pulls/9/reviews/%d --input -", id)
}

func TestGHPostSubmitsWithoutAShell(t *testing.T) {
	calls := recordingGH(t, "")
	body := "it's `fine` $(whoami)\n\"quoted\""
	if err := GHPost(context.Background(), "a/b", 9, "APPROVE", shaA, body); err != nil {
		t.Fatal(err)
	}
	w := writes(calls())
	if len(w) != 1 || w[0].args != createPath {
		t.Fatalf("writes = %+v", w)
	}
	if w[0].input["body"] != body || w[0].input["event"] != "APPROVE" || w[0].input["commit_id"] != shaA {
		t.Errorf("payload = %v", w[0].input)
	}
	if err := GHPost(context.Background(), "a/b", 9, "REQUEST_CHANGES", shaA, ""); err == nil {
		t.Error("request changes accepted")
	}
}

func TestGHPostUpdatesItsCommentInPlace(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name, event, reviews, body string
		wantErr                    error
		want                       []string // write paths, in order
		wantInBody                 string
	}{
		{name: "first comment is added", event: EventUpdate, body: "fix it",
			want: []string{createPath}, wantInBody: "fix it"},
		{name: "later comment edits it", event: EventUpdate, body: "new",
			reviews: review(1, "COMMENTED", "me", mine), want: []string{editPath(1)}, wantInBody: "new"},
		{name: "login case does not matter", event: EventUpdate, body: "new",
			reviews: review(1, "COMMENTED", "ME", mine), want: []string{editPath(1)}, wantInBody: "new"},
		{name: "newest of several", event: EventUpdate, body: "new",
			reviews: review(1, "COMMENTED", "me", mine) + review(2, "COMMENTED", "me", mine),
			want:    []string{editPath(2)}, wantInBody: "new"},
		{name: "someone else's marker does not count", event: EventUpdate, body: "new",
			reviews: review(1, "COMMENTED", "mallory", mine), want: []string{createPath}, wantInBody: "new"},
		{name: "the user's own review does not count", event: EventUpdate, body: "new",
			reviews: review(1, "COMMENTED", "me", "by hand"), want: []string{createPath}, wantInBody: "new"},
		{name: "a quoted marker does not count", event: EventUpdate, body: "new",
			reviews: review(1, "COMMENTED", "me", "see "+commentMarker), want: []string{createPath}, wantInBody: "new"},
		{name: "an approval is not edited", event: EventUpdate, body: "new",
			reviews: review(1, "APPROVED", "me", mine), want: []string{createPath}, wantInBody: "new"},
		{name: "a plain comment always adds", event: "COMMENT", body: "new",
			reviews: review(1, "COMMENTED", "me", mine), want: []string{createPath}, wantInBody: "new"},
		{name: "refresh with nothing to refresh adds none", event: EventRefresh, body: "ok", wantErr: ErrNoComment},
		{name: "refresh edits", event: EventRefresh, body: "ok",
			reviews: review(1, "COMMENTED", "me", mine), want: []string{editPath(1)}, wantInBody: "ok"},
		{name: "refresh with no text says so", event: EventRefresh,
			reviews: review(1, "COMMENTED", "me", mine), want: []string{editPath(1)}, wantInBody: "nothing further"},
		{name: "an approval is its own review", event: "APPROVE", body: "good",
			reviews: review(1, "COMMENTED", "me", mine), want: []string{createPath}, wantInBody: "good"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := recordingGH(t, tt.reviews)
			if err := GHPost(ctx, "a/b", 9, tt.event, shaB, tt.body); !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v", err)
			}
			w := writes(calls())
			if len(w) != len(tt.want) {
				t.Fatalf("writes = %+v, want %v", w, tt.want)
			}
			for i, c := range w {
				if c.args != tt.want[i] {
					t.Errorf("write %d = %q, want %q", i, c.args, tt.want[i])
				}
			}
			if len(w) == 0 {
				return
			}
			last := w[len(w)-1]
			got := last.input["body"]
			if !strings.Contains(got, tt.wantInBody) {
				t.Errorf("body = %q", got)
			}
			if tt.event != "APPROVE" {
				if !strings.HasPrefix(got, commentMarker+"\n") || !strings.Contains(got, shaB) {
					t.Errorf("not marked with the commit: %q", got)
				}
			}
		})
	}
}

func TestGHPostNeverDuplicatesWhenItCannotLook(t *testing.T) {
	ctx := context.Background()
	for _, failing := range []string{"user", "--paginate"} {
		calls := recordingGH(t, "", failing)
		if err := GHPost(ctx, "a/b", 9, EventUpdate, shaA, "x"); err == nil || errors.Is(err, ErrNoComment) {
			t.Fatalf("%s: err = %v", failing, err)
		}
		if w := writes(calls()); len(w) != 0 {
			t.Fatalf("%s: writes = %+v", failing, w)
		}
	}
	calls := recordingGH(t, "not json")
	if err := GHPost(ctx, "a/b", 9, EventRefresh, shaA, "x"); err == nil || errors.Is(err, ErrNoComment) {
		t.Fatalf("malformed reviews: err = %v", err)
	}
	if w := writes(calls()); len(w) != 0 {
		t.Fatalf("malformed reviews: writes = %+v", w)
	}
}

func TestGHPostFailedEditAddsNoComment(t *testing.T) {
	calls := recordingGH(t, review(1, "COMMENTED", "me", mine), "PUT")
	if err := GHPost(context.Background(), "a/b", 9, EventUpdate, shaA, "new"); err == nil {
		t.Fatal("a failed edit was not reported")
	}
	if w := writes(calls()); len(w) != 1 {
		t.Fatalf("a failed edit added a comment: %+v", w)
	}
}
