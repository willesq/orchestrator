package docs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeTool puts an executable named name on PATH that appends one line per
// run to the returned log: the GH_CONFIG_DIR and GIT_CONFIG_GLOBAL it saw,
// its arguments, and the names (never the values) of its environment. It then
// prints out.
func fakeTool(t *testing.T, name, out string) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, name+".log")
	script := "#!/bin/sh\n" +
		"{ printf 'dir=%s|global=%s|args=%s|names=' \"$GH_CONFIG_DIR\" \"$GIT_CONFIG_GLOBAL\" \"$*\"; env | cut -d= -f1 | tr '\\n' ' '; echo; } >> '" + logPath + "'\n" +
		"printf '%s' '" + out + "'\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func logLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestProfileGitHubListsAsTheProfile(t *testing.T) {
	logPath := fakeTool(t, "gh", `{"data":{"repositoryOwner":{"repositories":{"pageInfo":{"hasNextPage":false},"nodes":[{"name":"docs"}]}}}}`)
	env := append(slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "GH_CONFIG_DIR=") }), "GH_CONFIG_DIR=/profiles/acme")
	repos, err := NewProfileGitHub(env).ListRepos(context.Background(), "acme")
	if err != nil || len(repos) != 1 || repos[0].Name != "docs" {
		t.Fatalf("ListRepos = %v, %v", repos, err)
	}
	lines := logLines(t, logPath)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "dir=/profiles/acme|") || !strings.Contains(lines[0], "owner=acme") {
		t.Fatalf("gh calls = %q", lines)
	}
}

// TestProfileGitClonesAsTheProfile: a profile's clone runs git over HTTPS
// with gh, in the profile's environment, as the only credential helper, and
// without the user's global or system git configuration.
func TestProfileGitClonesAsTheProfile(t *testing.T) {
	logPath := fakeTool(t, "git", "")
	env := []string{"PATH=" + os.Getenv("PATH"), "GH_CONFIG_DIR=/profiles/acme"}
	dest := filepath.Join(t.TempDir(), "acme", "docs")
	if err := NewProfileGit(env).Sync(context.Background(), "acme", "docs", dest); err != nil {
		t.Fatal(err)
	}
	lines := logLines(t, logPath)
	if len(lines) != 1 {
		t.Fatalf("git calls = %q", lines)
	}
	line := lines[0]
	wantArgs := "args=-c credential.helper= -c credential.helper=!gh auth git-credential clone --depth 1 --progress -- https://github.com/acme/docs.git " + dest + "|"
	if !strings.HasPrefix(line, "dir=/profiles/acme|global=/dev/null|") || !strings.Contains(line, wantArgs) {
		t.Errorf("git call = %q", line)
	}
	for _, name := range []string{"GIT_CONFIG_NOSYSTEM", "GIT_TERMINAL_PROMPT"} {
		if !strings.Contains(line, " "+name+" ") {
			t.Errorf("git ran without %s: %q", name, line)
		}
	}
}

func TestProfileGitRefusesBadNames(t *testing.T) {
	g := ghGit{profile: true, run: func(context.Context, string, ...string) ([]byte, error) {
		t.Error("ran a clone for an invalid repo")
		return nil, nil
	}}
	if err := g.Sync(context.Background(), "acme", "../x", filepath.Join(t.TempDir(), "d")); err == nil {
		t.Error("Sync accepted an invalid repo name")
	}
}

// TestSyncOwnerClientsRefuses is SP-063.1.a2 for docs: an owner whose
// Clients refuses (its isolated space has no logged-in gh profile) is not
// listed, cloned or ingested, and its status says why.
func TestSyncOwnerClientsRefuses(t *testing.T) {
	gh := &countingGH{}
	git := &fakeGit{}
	reg, ing, opts := syncDeps(t, gh, git)
	opts.Clients = func(context.Context, string) (GitHub, Git, error) {
		return nil, nil, errors.New("its space has no gh profile")
	}
	res := SyncOwner(context.Background(), reg, Owner{Owner: "acme"}, ing, opts)
	if res.Status != "error" || !strings.Contains(res.Reason, "no gh profile") {
		t.Errorf("result = %+v", res)
	}
	if gh.calls != 0 || len(git.synced) != 0 || len(ing.uris) != 0 {
		t.Errorf("listed %d times, cloned %v, ingested %v", gh.calls, git.synced, ing.uris)
	}
}

// TestSyncOwnerUsesClients: the GitHub and Git Clients returns for the owner
// replace the defaults for that sync.
func TestSyncOwnerUsesClients(t *testing.T) {
	unused := &countingGH{}
	reg, ing, opts := syncDeps(t, unused, &fakeGit{})
	profGit := &fakeGit{layout: map[string]map[string]string{"docs": {"README.md": "hi"}}}
	var asked string
	opts.Clients = func(_ context.Context, owner string) (GitHub, Git, error) {
		asked = owner
		return fakeGH{repos: []Repo{{Name: "docs", HeadSHA: "s1"}}}, profGit, nil
	}
	res := SyncOwner(context.Background(), reg, Owner{Owner: "acme"}, ing, opts)
	if res.Status != "ok" || asked != "acme" || unused.calls != 0 || !slices.Equal(profGit.synced, []string{"docs"}) {
		t.Errorf("result = %+v, asked %q, default listed %d, profile cloned %v", res, asked, unused.calls, profGit.synced)
	}
}

type countingGH struct{ calls int }

func (c *countingGH) ListRepos(context.Context, string) ([]Repo, error) {
	c.calls++
	return nil, nil
}
