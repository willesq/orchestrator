package docs

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func TestOwnerCovers(t *testing.T) {
	tests := []struct {
		name string
		o    Owner
		repo string
		want bool
	}{
		{"empty mode is all", Owner{}, "x", true},
		{"all", Owner{RepoMode: ReposAll}, "x", true},
		{"include listed", Owner{RepoMode: ReposInclude, Repos: []string{"x"}}, "x", true},
		{"include is case-insensitive", Owner{RepoMode: ReposInclude, Repos: []string{"X"}}, "x", true},
		{"include others", Owner{RepoMode: ReposInclude, Repos: []string{"x"}}, "y", false},
		{"exclude listed", Owner{RepoMode: ReposExclude, Repos: []string{"x"}}, "x", false},
		{"exclude others", Owner{RepoMode: ReposExclude, Repos: []string{"x"}}, "y", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.o.Covers(tt.repo); got != tt.want {
				t.Errorf("Covers = %v", got)
			}
		})
	}
}

func TestChoiceValidate(t *testing.T) {
	tests := []struct {
		name string
		c    Choice
		ok   bool
	}{
		{"all", Choice{RepoMode: ReposAll}, true},
		{"include", Choice{RepoMode: ReposInclude, Repos: []string{"a.b-c_d"}}, true},
		{"all with repos", Choice{RepoMode: ReposAll, Repos: []string{"a"}}, false},
		{"include without repos", Choice{RepoMode: ReposInclude}, false},
		{"exclude without repos", Choice{RepoMode: ReposExclude}, false},
		{"unknown mode", Choice{RepoMode: "some"}, false},
		{"empty mode", Choice{}, false},
		{"comma would split the stored list", Choice{RepoMode: ReposInclude, Repos: []string{"a,b"}}, false},
		{"path in a name", Choice{RepoMode: ReposInclude, Repos: []string{"../x"}}, false},
		{"negative refresh", Choice{RepoMode: ReposAll, RefreshSecs: -1}, false},
		{"refresh too often", Choice{RepoMode: ReposAll, RefreshSecs: 59}, false},
		{"refresh at the floor", Choice{RepoMode: ReposAll, RefreshSecs: 60}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.c.Validate(); (err == nil) != tt.ok {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func repoState(t *testing.T, reg *Registry, owner, repo string) RepoState {
	t.Helper()
	repos, err := reg.ListRepos(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range repos {
		if r.Repo == repo {
			return r
		}
	}
	t.Fatalf("no repo %s/%s", owner, repo)
	return RepoState{}
}

func TestSetChoice(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) *Registry {
		t.Helper()
		reg := testRegistry(t)
		if err := reg.AddOwner(ctx, Owner{Owner: "acme", RefreshSecs: 60}); err != nil {
			t.Fatal(err)
		}
		if err := reg.MarkSynced(ctx, "acme", 1000, "partial", "1 of 2 repos failed"); err != nil {
			t.Fatal(err)
		}
		for _, r := range []RepoState{
			{Owner: "acme", Repo: "good", HeadSHA: "g1", Files: 3, Status: "ok"},
			{Owner: "acme", Repo: "bad", HeadSHA: "b1", Status: "error", Error: "clone failed"},
		} {
			if err := reg.SetRepoState(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		return reg
	}

	t.Run("a new owner is on and covers everything", func(t *testing.T) {
		reg := setup(t)
		owners, _ := reg.ListOwners(ctx)
		if !owners[0].Enabled || owners[0].RepoMode != ReposAll || owners[0].Repos != nil {
			t.Errorf("owner = %+v", owners[0])
		}
	})
	t.Run("an unknown owner", func(t *testing.T) {
		reg := setup(t)
		if _, err := reg.SetChoice(ctx, "nobody", Choice{RepoMode: ReposAll}); !errors.Is(err, ErrNoOwner) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("an invalid choice changes nothing", func(t *testing.T) {
		reg := setup(t)
		if _, err := reg.SetChoice(ctx, "acme", Choice{RepoMode: "some"}); err == nil {
			t.Fatal("accepted")
		}
		if got := repoState(t, reg, "acme", "bad"); got.Status != "error" {
			t.Errorf("state = %+v", got)
		}
	})
	t.Run("excluding keeps the repo's state but clears its failure", func(t *testing.T) {
		reg := setup(t)
		o, err := reg.SetChoice(ctx, "acme", Choice{Enabled: true, RepoMode: ReposExclude, Repos: []string{"bad"}})
		if err != nil {
			t.Fatal(err)
		}
		if o.LastSynced != 0 {
			t.Errorf("a changed selection should make the owner due: %+v", o)
		}
		got := repoState(t, reg, "acme", "bad")
		if got.Status != StatusExcluded || got.Error != "" || got.HeadSHA != "b1" {
			t.Errorf("bad = %+v", got)
		}
		if got := repoState(t, reg, "acme", "good"); got.Status != "ok" || got.HeadSHA != "g1" {
			t.Errorf("good = %+v", got)
		}
	})
	t.Run("covering a repo again makes the next sync walk it", func(t *testing.T) {
		reg := setup(t)
		if _, err := reg.SetChoice(ctx, "acme", Choice{Enabled: true, RepoMode: ReposInclude, Repos: []string{"good"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := reg.SetChoice(ctx, "acme", Choice{Enabled: true, RepoMode: ReposAll}); err != nil {
			t.Fatal(err)
		}
		got := repoState(t, reg, "acme", "bad")
		if got.Status != "new" || got.HeadSHA != "" {
			t.Errorf("bad = %+v", got)
		}
	})
	t.Run("pausing is not due and clears what needs attention, and resuming is due", func(t *testing.T) {
		reg := setup(t)
		o, err := reg.SetChoice(ctx, "acme", Choice{RepoMode: ReposAll})
		if err != nil {
			t.Fatal(err)
		}
		if o.Enabled || o.Status != StatusPaused || o.Error != "" {
			t.Errorf("paused owner = %+v", o)
		}
		if err := reg.MarkDue(ctx, "acme"); err != nil {
			t.Fatal(err)
		}
		due, err := reg.DueOwners(ctx, 1<<40)
		if err != nil || len(due) != 0 {
			t.Errorf("a paused owner came due: %+v, %v", due, err)
		}
		if got := repoState(t, reg, "acme", "good"); got.Status != StatusExcluded {
			t.Errorf("good = %+v", got)
		}
		o, err = reg.SetChoice(ctx, "acme", Choice{Enabled: true, RepoMode: ReposAll})
		if err != nil {
			t.Fatal(err)
		}
		if o.Status != "new" || o.LastSynced != 0 {
			t.Errorf("resumed owner = %+v", o)
		}
		due, err = reg.DueOwners(ctx, 1<<40)
		if err != nil || len(due) != 1 || !due[0].Enabled {
			t.Errorf("due = %+v, %v", due, err)
		}
	})
	t.Run("only a new cadence moves the cadence", func(t *testing.T) {
		reg := setup(t)
		o, err := reg.SetChoice(ctx, "acme", Choice{Enabled: true, RepoMode: ReposAll})
		if err != nil {
			t.Fatal(err)
		}
		if o.RefreshSecs != 60 || o.LastSynced != 1000 {
			t.Errorf("an unchanged save moved things: %+v", o)
		}
		if o, _ = reg.SetChoice(ctx, "acme", Choice{Enabled: true, RepoMode: ReposAll, RefreshSecs: 90}); o.RefreshSecs != 90 {
			t.Errorf("refresh = %d", o.RefreshSecs)
		}
	})
	t.Run("the selection survives a reload", func(t *testing.T) {
		reg := setup(t)
		if _, err := reg.SetChoice(ctx, "acme", Choice{Enabled: true, RepoMode: ReposInclude, Repos: []string{"a", "b"}}); err != nil {
			t.Fatal(err)
		}
		owners, _ := reg.ListOwners(ctx)
		if owners[0].RepoMode != ReposInclude || !slices.Equal(owners[0].Repos, []string{"a", "b"}) {
			t.Errorf("owner = %+v", owners[0])
		}
	})
}

func TestForgetRepo(t *testing.T) {
	ctx := context.Background()
	reg := testRegistry(t)
	if err := reg.SetRepoState(ctx, RepoState{Owner: "acme", Repo: "a", Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	for _, uri := range []string{"repo://acme/a/x.md", "repo://acme/a-b/x.md", "repo://acme/b/x.md"} {
		if err := reg.SetHash(ctx, uri, "h", 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.ForgetRepo(ctx, "acme", "a"); err != nil {
		t.Fatal(err)
	}
	if repos, _ := reg.ListRepos(ctx, "acme"); len(repos) != 0 {
		t.Errorf("repos = %+v", repos)
	}
	docs, _ := reg.ListDocs(ctx)
	var uris []string
	for _, d := range docs {
		uris = append(uris, d.URI)
	}
	if !slices.Equal(uris, []string{"repo://acme/a-b/x.md", "repo://acme/b/x.md"}) {
		t.Errorf("left %v; a repo sharing a name prefix must keep its hashes", uris)
	}
}

func TestSyncSkipsReposTheOwnerDoesNotCover(t *testing.T) {
	ctx := context.Background()
	reg := testRegistry(t)
	gh := fakeGH{repos: []Repo{{Name: "in", HeadSHA: "s1"}, {Name: "out", HeadSHA: "s2"}}}
	git := &fakeGit{}
	res := SyncOwner(ctx, reg, Owner{Owner: "acme", RepoMode: ReposExclude, Repos: []string{"out"}}, &fakeIng{},
		SyncOpts{RepoCache: t.TempDir(), GH: gh, Git: git, Now: func() int64 { return 1 }})
	if !slices.Equal(git.synced, []string{"in"}) || res.Repos != 1 {
		t.Errorf("cloned %v, result %+v", git.synced, res)
	}
}

func TestOwnerSpellings(t *testing.T) {
	ctx := context.Background()
	reg := testRegistry(t)
	if err := reg.AddOwner(ctx, Owner{Owner: "acme", RefreshSecs: 60}); err != nil {
		t.Fatal(err)
	}
	if err := reg.AddOwner(ctx, Owner{Owner: "ACME", RefreshSecs: 90}); err != nil {
		t.Fatal(err)
	}
	owners, _ := reg.ListOwners(ctx)
	if len(owners) != 1 || owners[0].Owner != "acme" || owners[0].RefreshSecs != 90 {
		t.Fatalf("owners = %+v", owners)
	}
	o, err := reg.SetChoice(ctx, "Acme", Choice{Enabled: true, RepoMode: ReposAll})
	if err != nil || o.Owner != "acme" {
		t.Fatalf("SetChoice = %+v, %v", o, err)
	}
}

func TestRemoveOwnerLeavesOtherSpellingsAndLookalikes(t *testing.T) {
	ctx := context.Background()
	reg := testRegistry(t)
	for _, uri := range []string{"repo://a_b/r/x.md", "repo://axb/r/x.md", "repo://A_B/r/x.md", "repo://a_b2/r/x.md"} {
		if err := reg.SetHash(ctx, uri, "h", 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.RemoveOwner(ctx, "a_b"); err != nil {
		t.Fatal(err)
	}
	docs, _ := reg.ListDocs(ctx)
	var uris []string
	for _, d := range docs {
		uris = append(uris, d.URI)
	}
	if !slices.Equal(uris, []string{"repo://A_B/r/x.md", "repo://a_b2/r/x.md", "repo://axb/r/x.md"}) {
		t.Errorf("left %v", uris)
	}
}

func TestClearCachesKeepsWhatOptedOutReposHold(t *testing.T) {
	ctx := context.Background()
	for _, owner := range []string{"acme", ""} {
		t.Run("owner="+owner, func(t *testing.T) {
			reg := testRegistry(t)
			if err := reg.AddOwner(ctx, Owner{Owner: "acme", RefreshSecs: 60}); err != nil {
				t.Fatal(err)
			}
			for _, repo := range []string{"live", "out"} {
				if err := reg.SetRepoState(ctx, RepoState{Owner: "acme", Repo: repo, HeadSHA: "h", Status: "ok"}); err != nil {
					t.Fatal(err)
				}
				if err := reg.SetHash(ctx, "repo://acme/"+repo+"/x.md", "h", 1); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := reg.SetChoice(ctx, "acme", Choice{Enabled: true, RepoMode: ReposExclude, Repos: []string{"out"}}); err != nil {
				t.Fatal(err)
			}
			if err := reg.ClearCaches(ctx, owner); err != nil {
				t.Fatal(err)
			}
			if h, _ := reg.Hash(ctx, "repo://acme/live/x.md"); h != "" {
				t.Error("a synced repo's hash survived a forced sync")
			}
			if h, _ := reg.Hash(ctx, "repo://acme/out/x.md"); h == "" {
				t.Error("an opted-out repo lost its hash, so a purge could not find its docs")
			}
			repos, _ := reg.ListRepos(ctx, "acme")
			if len(repos) != 1 || repos[0].Repo != "out" || repos[0].Status != StatusExcluded {
				t.Errorf("repos = %+v", repos)
			}
		})
	}
}
