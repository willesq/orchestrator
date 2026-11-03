package prs

import (
	"context"
	"encoding/json"
	"log"
	"slices"
	"sort"
	"strings"

	"github.com/willesq/orchestrator/internal/github"
)

// Watch is what the babysitter may report: the user's login and the scopes
// they configured.
type Watch struct {
	Login  string
	Scopes []github.Scope
}

// WatchSource reports the current Watch.
type WatchSource func(ctx context.Context) (Watch, error)

// searchLimit caps each search: they run every sweep, and GitHub allows 30
// searches a minute.
const searchLimit = "100"

// MaxEverythingScopes caps the scopes watched for every open item: each adds
// two searches to every sweep. Beyond it (alphabetically), a scope is watched
// only for items involving the user.
const MaxEverythingScopes = 5

// MaxEverythingRepoBatches caps the batches of repos watched for every open
// item: each adds two searches to every sweep. Beyond it (alphabetically), a
// repo is watched only for items involving the user.
const MaxEverythingRepoBatches = 5

// maxRepoBatchChars keeps a batch's repo: qualifiers under GitHub's limit of
// 256 characters a query, leaving room for the qualifiers gh adds.
const maxRepoBatchChars = 200

// repoSearchMissing is how GitHub answers a search whose repos are all gone or
// out of reach.
const repoSearchMissing = "cannot be searched"

// query is one gh search. reason tags every hit, or, when empty, the hit's
// author and assignees decide it (involvement).
type query struct {
	kind   string
	flags  []string
	reason string
	// skipMissing tolerates repos that no longer exist, so one the user listed
	// and later deleted cannot stop every sweep.
	skipMissing bool
}

// ghUser is a GitHub account in search results.
type ghUser struct {
	Login string `json:"login"`
}

// hit is one search result.
type hit struct {
	URL        string `json:"url"`
	Title      string `json:"title"`
	Number     int    `json:"number"`
	IsDraft    bool   `json:"isDraft"`
	UpdatedAt  string `json:"updatedAt"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Author    ghUser   `json:"author"`
	Assignees []ghUser `json:"assignees"`
}

// reasonOrder keeps an item's reasons in one order, so an unchanged item
// compares equal between sweeps.
var reasonOrder = []string{ReasonAuthored, ReasonAssigned, ReasonReviewRequested, ReasonInvolved, ReasonAllOpen}

// poll finds open PRs and issues in the enabled scopes. A fixed set of
// searches covers everything involving the user, whatever the number of
// scopes; only scopes and repos watched for everything add searches of their
// own. Hits outside the enabled scopes, or their repo picks, are dropped.
func (b *Babysitter) poll(ctx context.Context) ([]PR, error) {
	w, err := b.watch(ctx)
	if err != nil {
		return nil, err
	}
	watched := map[string]github.Scope{}
	var everything, everythingRepos []string
	for _, sc := range w.Scopes {
		if !sc.Enabled || !github.ValidLogin(sc.Owner) {
			continue
		}
		watched[strings.ToLower(sc.Owner)] = sc
		if sc.Items == github.ItemsAll {
			everything = append(everything, sc.Owner)
		}
		for _, repo := range sc.WatchedForEverything() {
			if github.ValidRepo(repo) {
				everythingRepos = append(everythingRepos, sc.Owner+"/"+repo)
			}
		}
	}
	if len(watched) == 0 {
		return nil, nil
	}
	sort.Strings(everything)
	if len(everything) > MaxEverythingScopes {
		everything = everything[:MaxEverythingScopes]
	}

	queries := []query{
		{kind: KindPR, flags: []string{"--involves=@me"}},
		// Includes requests made to a team the user is on.
		{kind: KindPR, flags: []string{"--review-requested=@me"}, reason: ReasonReviewRequested},
		{kind: KindIssue, flags: []string{"--involves=@me"}},
	}
	for _, owner := range everything {
		queries = append(queries,
			query{kind: KindPR, flags: []string{"--owner=" + owner}, reason: ReasonAllOpen},
			query{kind: KindIssue, flags: []string{"--owner=" + owner}, reason: ReasonAllOpen})
	}
	for _, flags := range repoBatches(everythingRepos) {
		queries = append(queries,
			query{kind: KindPR, flags: flags, reason: ReasonAllOpen, skipMissing: true},
			query{kind: KindIssue, flags: flags, reason: ReasonAllOpen, skipMissing: true})
	}

	byURL := map[string]*PR{}
	var order []string
	for _, q := range queries {
		hits, err := b.search(ctx, q)
		if err != nil && q.skipMissing && strings.Contains(err.Error(), repoSearchMissing) {
			log.Printf("pr-babysitter: skipping unsearchable repos %v: %v", q.flags, err)
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, h := range hits {
			// Only github.com item URLs: one later goes to gh pr view as an
			// argument, so nothing that could read as a flag.
			if !strings.HasPrefix(h.URL, "https://github.com/") {
				continue
			}
			owner, repo, ok := strings.Cut(h.Repository.NameWithOwner, "/")
			sc, watchedOwner := watched[strings.ToLower(owner)]
			if !ok || !watchedOwner || !sc.Watches(repo) {
				continue
			}
			item, seen := byURL[h.URL]
			if !seen {
				item = &PR{
					URL: h.URL, Title: h.Title, Repo: h.Repository.NameWithOwner, Kind: q.kind,
					Number: h.Number, Author: h.Author.Login, UpdatedAt: h.UpdatedAt, IsDraft: h.IsDraft,
				}
				byURL[h.URL] = item
				order = append(order, h.URL)
			}
			reasons := []string{q.reason}
			if q.reason == "" {
				reasons = involvement(h, w.Login)
			}
			for _, r := range reasons {
				if !slices.Contains(item.Reasons, r) {
					item.Reasons = append(item.Reasons, r)
				}
			}
		}
	}

	out := make([]PR, 0, len(order))
	for _, url := range order {
		item := byURL[url]
		sortReasons(item.Reasons)
		// CI and review state only matter where the PR can need the user.
		if item.IsPR() && (item.Authored() || slices.Contains(item.Reasons, ReasonReviewRequested)) {
			enrich(ctx, b.gh, item)
		}
		out = append(out, *item)
	}
	return out, nil
}

// repoBatches groups owner/name repos into the --repo flags of one search each,
// up to MaxEverythingRepoBatches, so the number of searches stays nearly fixed
// however long the lists grow.
func repoBatches(repos []string) [][]string {
	slices.SortFunc(repos, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	var batches [][]string
	var batch []string
	chars := 0
	for _, repo := range repos {
		cost := len("repo: ") + len(repo)
		if len(batch) > 0 && chars+cost > maxRepoBatchChars {
			batches = append(batches, batch)
			batch, chars = nil, 0
		}
		batch = append(batch, "--repo="+repo)
		chars += cost
	}
	if len(batch) > 0 {
		batches = append(batches, batch)
	}
	if len(batches) > MaxEverythingRepoBatches {
		batches = batches[:MaxEverythingRepoBatches]
	}
	return batches
}

// involvement says how an --involves hit relates to the user.
func involvement(h hit, login string) []string {
	var out []string
	if login != "" && strings.EqualFold(h.Author.Login, login) {
		out = append(out, ReasonAuthored)
	}
	if login != "" && slices.ContainsFunc(h.Assignees, func(a ghUser) bool { return strings.EqualFold(a.Login, login) }) {
		out = append(out, ReasonAssigned)
	}
	if len(out) == 0 {
		out = append(out, ReasonInvolved)
	}
	return out
}

func sortReasons(reasons []string) {
	sort.SliceStable(reasons, func(i, j int) bool {
		return slices.Index(reasonOrder, reasons[i]) < slices.Index(reasonOrder, reasons[j])
	})
}

func (b *Babysitter) search(ctx context.Context, q query) ([]hit, error) {
	fields := "url,title,number,repository,author,assignees,updatedAt"
	what := "issues"
	if q.kind == KindPR {
		fields += ",isDraft"
		what = "prs"
	}
	args := append([]string{"search", what}, q.flags...)
	// Newest first, so a burst of stale items cannot push recent ones past
	// the limit.
	args = append(args, "--state=open", "--sort=updated", "--limit", searchLimit, "--json", fields)
	out, err := b.gh(ctx, args...)
	if err != nil {
		return nil, err
	}
	var hits []hit
	if err := json.Unmarshal(out, &hits); err != nil {
		return nil, err
	}
	return hits, nil
}
