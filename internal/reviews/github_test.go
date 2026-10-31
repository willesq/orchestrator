package reviews

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func canned(out string, err error) func(context.Context, ...string) ([]byte, error) {
	return func(context.Context, ...string) ([]byte, error) { return []byte(out), err }
}

func TestFetchStatus(t *testing.T) {
	ctx := context.Background()
	st, err := FetchStatus(ctx, canned(`{"state":"OPEN","isDraft":false,"headRefOid":"`+shaA+`","mergeable":"MERGEABLE",
		"statusCheckRollup":[{"conclusion":"SUCCESS"},{"state":"SUCCESS"}],"author":{"login":"x"}}`, nil), testURL)
	if err != nil || !st.Ready() || st.CI != "success" || st.Author != "x" || st.Head != shaA {
		t.Fatalf("FetchStatus = %+v, %v", st, err)
	}
	if _, err := FetchStatus(ctx, canned("", errors.New("gh down")), testURL); err == nil {
		t.Error("gh error not returned")
	}
	if _, err := FetchStatus(ctx, canned("{", nil), testURL); err == nil {
		t.Error("bad json accepted")
	}
	if _, err := FetchStatus(ctx, canned(`{"state":"OPEN","headRefOid":"; rm -rf"}`, nil), testURL); err == nil {
		t.Error("non-SHA head accepted")
	}
}

func TestStatusWaiting(t *testing.T) {
	if got := (Status{State: "OPEN", Head: shaA, CI: "success"}).Waiting(); got != "waiting for the PR to be ready" {
		t.Errorf("Waiting = %q", got)
	}
	if (Status{State: "OPEN", CI: "success"}).Ready() {
		t.Error("a status without a head is ready")
	}
}

func TestFetchDetail(t *testing.T) {
	files := []map[string]any{}
	for i := 0; i < maxFiles+5; i++ {
		files = append(files, map[string]any{"path": "f" + strings.Repeat("x", i%3) + ".go", "additions": 1, "deletions": 2})
	}
	raw, _ := json.Marshal(map[string]any{
		"title": "Add ‮evil‬", "body": "hello\nworld", "author": map[string]string{"login": "dev"},
		"state": "OPEN", "isDraft": true, "baseRefName": "main", "headRefName": "feat/x", "headRefOid": shaA,
		"additions": 10, "deletions": 3, "changedFiles": 105, "createdAt": "2026-10-01T00:00:00Z",
		"updatedAt": "2026-10-02T00:00:00Z", "mergeable": "MERGEABLE", "reviewDecision": "REVIEW_REQUIRED",
		"labels":         []map[string]string{{"name": "bug"}},
		"reviewRequests": []map[string]string{{"login": "alice"}, {"slug": "core"}, {"name": "Team Name"}},
		"latestReviews":  []map[string]any{{"author": map[string]string{"login": "bob"}, "state": "APPROVED"}},
		"statusCheckRollup": []map[string]string{
			{"name": "test", "status": "COMPLETED", "conclusion": "SUCCESS"},
			{"context": "ci/legacy", "state": "PENDING"},
			{"name": "queued", "status": "QUEUED"},
		},
		"files": files,
	})
	d, err := FetchDetail(context.Background(), canned(string(raw), nil), testURL)
	if err != nil {
		t.Fatal(err)
	}
	if d.Title != "Add evil" || d.Body != "hello\nworld" || d.Author != "dev" || !d.IsDraft || d.CI != "pending" {
		t.Errorf("detail = %+v", d)
	}
	if len(d.Files) != maxFiles || d.ChangedFiles != 105 {
		t.Errorf("files = %d, changed %d", len(d.Files), d.ChangedFiles)
	}
	if strings.Join(d.RequestedReviewers, ",") != "alice,core,Team Name" || d.Labels[0] != "bug" ||
		d.Reviews[0] != (ReviewerState{"bob", "APPROVED"}) {
		t.Errorf("people = %+v %+v %+v", d.RequestedReviewers, d.Labels, d.Reviews)
	}
	want := []CheckState{{"test", "success"}, {"ci/legacy", "pending"}, {"queued", "queued"}}
	for i, c := range want {
		if d.Checks[i] != c {
			t.Errorf("check %d = %+v, want %+v", i, d.Checks[i], c)
		}
	}
	if _, err := FetchDetail(context.Background(), canned("", errors.New("x")), testURL); err == nil {
		t.Error("gh error not returned")
	}
	if _, err := FetchDetail(context.Background(), canned("[", nil), testURL); err == nil {
		t.Error("bad json accepted")
	}
}

// fakeGH puts a gh on PATH that records its arguments and stdin, and exits
// with code.
func fakeGH(t *testing.T, code int, stderr string) (argsFile, stdinFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile, stdinFile = filepath.Join(dir, "args"), filepath.Join(dir, "stdin")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\ncat > " + stdinFile + "\n"
	if stderr != "" {
		script += "printf '%s' '" + stderr + "' >&2\n"
	}
	script += "exit " + string(rune('0'+code)) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsFile, stdinFile
}

func TestGHPost(t *testing.T) {
	argsFile, stdinFile := fakeGH(t, 0, "")
	body := "it's `fine` $(whoami)\n\"quoted\""
	if err := GHPost(context.Background(), "a/b", 9, "APPROVE", shaA, body); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(argsFile)
	if got := strings.Fields(string(args)); strings.Join(got, " ") != "api --method POST repos/a/b/pulls/9/reviews --input -" {
		t.Errorf("args = %q", got)
	}
	var payload map[string]string
	stdin, _ := os.ReadFile(stdinFile)
	if err := json.Unmarshal(stdin, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["body"] != body || payload["event"] != "APPROVE" || payload["commit_id"] != shaA {
		t.Errorf("payload = %v", payload)
	}

	if err := GHPost(context.Background(), "a/b", 9, "REQUEST_CHANGES", shaA, ""); err == nil {
		t.Error("request changes accepted")
	}
}

func TestGHPostFailure(t *testing.T) {
	fakeGH(t, 1, "HTTP 422: Can not approve your own pull request")
	err := GHPost(context.Background(), "a/b", 9, "COMMENT", shaA, "x")
	if err == nil || !strings.Contains(err.Error(), "Can not approve") {
		t.Fatalf("err = %v", err)
	}
	fakeGH(t, 2, "")
	if err := GHPost(context.Background(), "a/b", 9, "COMMENT", shaA, "x"); err == nil {
		t.Fatal("silent failure not returned")
	}
}

func TestFetchStatusAssociationOnlyWithoutCI(t *testing.T) {
	ctx := context.Background()
	calls := 0
	gh := func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		if args[0] == "api" {
			return []byte("CONTRIBUTOR\n"), nil
		}
		return []byte(`{"state":"OPEN","headRefOid":"` + shaA + `","statusCheckRollup":[]}`), nil
	}
	st, err := FetchStatus(ctx, gh, testURL)
	if err != nil || st.Association != "CONTRIBUTOR" || calls != 2 || st.ApproveReady() {
		t.Fatalf("no CI: %+v, %v, %d calls", st, err, calls)
	}
	failing := func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "api" {
			return nil, errors.New("HTTP 404")
		}
		return []byte(`{"state":"OPEN","headRefOid":"` + shaA + `"}`), nil
	}
	if _, err := FetchStatus(ctx, failing, testURL); err == nil {
		t.Fatal("association failure not returned")
	}
	if _, err := FetchStatus(ctx, canned(`{"state":"OPEN","headRefOid":"`+shaA+`"}`, nil), "https://github.com/a/b/issues/1"); err == nil {
		t.Fatal("bad URL accepted")
	}
}
