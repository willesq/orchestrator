package prs

import (
	"context"
	"encoding/json"
	"errors"
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
	// Accounts are the spaces' gh profiles, each searched as its own login
	// besides the default one (SP-063).
	Accounts []Account
	// Route names the account an owner's items come through: "" for the
	// default login, else an Account's Name. ok false drops the owner's
	// items, as its space has no working gh profile. nil routes every owner
	// to the default login.
	Route func(owner string) (account string, ok bool)
	// CheckOwner, when set, re-checks an owner's GitHub org binding once a
	// sweep before its items are cached (D-52); an owner it refuses has none
	// of its items kept.
	CheckOwner func(ctx context.Context, owner string) error
	// Space, when set, names the space an owner's items belong to under
	// the applied policy; each cached item and its events carry it
	// (SP-026).
	Space func(owner string) string
}

// Account is a gh login the babysitter searches as.
type Account struct {
	Name  string
	Login string
	GH    func(ctx context.Context, args ...string) ([]byte, error)
	// Deferred skips the account's searches this sweep, keeping the items
	// routed to it from the last one: its hourly GitHub budget is spent
	// (SP-074). A search that fails with github.ErrQuota defers it too.
	Deferred bool
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

	route := w.Route
	if route == nil {
		route = func(string) (string, bool) { return "", true }
	}
	byURL := map[string]*PR{}
	via := map[string]ghRunner{}
	var order []string
	// Reason: the PR cache is readable by tasks in any space, so a profile's
	// items, like the default login's, are kept only for a scope the user
	// enabled.
	kept := func(owner, repo string) bool {
		sc, ok := watched[strings.ToLower(owner)]
		return ok && sc.Watches(repo)
	}
	deferred := map[string]bool{}
	for _, acct := range append([]Account{{Login: w.Login, GH: b.gh}}, w.Accounts...) {
		if acct.Deferred && acct.Name != "" {
			deferred[acct.Name] = true
			continue
		}
		// Reason: each owner's items come only through its space's account,
		// so a call about one space's org never runs as another's login.
		mine := func(owner string) bool {
			name, ok := route(owner)
			return ok && name == acct.Name
		}
		err := b.searchAs(ctx, acct, mine, kept, everything, everythingRepos, byURL, via, &order)
		if errors.Is(err, github.ErrQuota) && acct.Name != "" {
			deferred[acct.Name] = true
			continue
		}
		if err != nil {
			return nil, err
		}
	}
	if len(deferred) > 0 {
		b.keepDeferred(route, kept, deferred, byURL, &order)
	}

	out := make([]PR, 0, len(order))
	refused := ownerChecks(ctx, w.CheckOwner)
	dropped := 0
	for _, url := range order {
		item := byURL[url]
		owner, _, _ := strings.Cut(item.Repo, "/")
		if refused(owner) {
			dropped++
			continue
		}
		// Reason: every item is stamped again, so one kept from an earlier
		// sweep carries its owner's current space.
		item.Space = ""
		if w.Space != nil {
			item.Space = w.Space(owner)
		}
		sortReasons(item.Reasons)
		// CI and review state only matter where the PR can need the user.
		if via[url] != nil && item.IsPR() && (item.Authored() || slices.Contains(item.Reasons, ReasonReviewRequested)) {
			enrich(ctx, via[url], item)
		}
		out = append(out, *item)
	}
	if dropped > 0 {
		// Reason: tasks can read orchd's log, so the line names no org.
		log.Printf("pr-babysitter: %d item(s) left out: their org's space binding is not confirmed", dropped)
	}
	return out, nil
}

// keepDeferred adds to byURL the last sweep's items routed to a deferred
// account and still in an enabled scope, so a spent budget does not read as
// the items closing. They keep their last CI and review state.
func (b *Babysitter) keepDeferred(route func(string) (string, bool), kept func(owner, repo string) bool,
	deferred map[string]bool, byURL map[string]*PR, order *[]string) {
	old := LoadState(b.stateFile).PRs
	urls := make([]string, 0, len(old))
	for url := range old {
		urls = append(urls, url)
	}
	sort.Strings(urls)
	n := 0
	for _, url := range urls {
		item := old[url]
		owner, repo, _ := strings.Cut(item.Repo, "/")
		name, ok := route(owner)
		if _, seen := byURL[url]; seen || !ok || !deferred[name] || !kept(owner, repo) {
			continue
		}
		byURL[url] = &item
		*order = append(*order, url)
		n++
	}
	// Reason: tasks can read orchd's log, so the line names no account.
	log.Printf("pr-babysitter: %d account(s) spent their hourly GitHub budget; %d of their item(s) kept from the last sweep", len(deferred), n)
}

// ownerChecks returns whether an owner is refused, asking check at most once
// per owner for the sweep.
func ownerChecks(ctx context.Context, check func(context.Context, string) error) func(owner string) bool {
	seen := map[string]bool{}
	return func(owner string) bool {
		if check == nil {
			return false
		}
		key := strings.ToLower(owner)
		refused, ok := seen[key]
		if !ok {
			refused = check(ctx, owner) != nil
			seen[key] = refused
		}
		return refused
	}
}

// searchAs runs the sweep's searches as acct, for the owners mine accepts,
// keeping the hits kept accepts and adding them to byURL (in order) and recording in via which account found
// each.
func (b *Babysitter) searchAs(ctx context.Context, acct Account, mine func(owner string) bool, kept func(owner, repo string) bool,
	everything, everythingRepos []string, byURL map[string]*PR, via map[string]ghRunner, order *[]string) error {
	queries := []query{
		{kind: KindPR, flags: []string{"--involves=@me"}},
		// Includes requests made to a team the user is on.
		{kind: KindPR, flags: []string{"--review-requested=@me"}, reason: ReasonReviewRequested},
		{kind: KindIssue, flags: []string{"--involves=@me"}},
	}
	for _, owner := range everything {
		if mine(owner) {
			queries = append(queries,
				query{kind: KindPR, flags: []string{"--owner=" + owner}, reason: ReasonAllOpen},
				query{kind: KindIssue, flags: []string{"--owner=" + owner}, reason: ReasonAllOpen})
		}
	}
	var repos []string
	for _, r := range everythingRepos {
		if owner, _, _ := strings.Cut(r, "/"); mine(owner) {
			repos = append(repos, r)
		}
	}
	for _, flags := range repoBatches(repos) {
		queries = append(queries,
			query{kind: KindPR, flags: flags, reason: ReasonAllOpen, skipMissing: true},
			query{kind: KindIssue, flags: flags, reason: ReasonAllOpen, skipMissing: true})
	}

	for _, q := range queries {
		hits, err := search(ctx, acct.GH, q)
		if err != nil && q.skipMissing && strings.Contains(err.Error(), repoSearchMissing) {
			log.Printf("pr-babysitter: skipping unsearchable repos %v: %v", q.flags, err)
			continue
		}
		if err != nil {
			return err
		}
		for _, h := range hits {
			// Only github.com item URLs: one later goes to gh pr view as an
			// argument, so nothing that could read as a flag.
			if !strings.HasPrefix(h.URL, "https://github.com/") {
				continue
			}
			owner, repo, ok := strings.Cut(h.Repository.NameWithOwner, "/")
			if !ok || !kept(owner, repo) || !mine(owner) {
				continue
			}
			item, seen := byURL[h.URL]
			if !seen {
				item = &PR{
					URL: h.URL, Title: h.Title, Repo: h.Repository.NameWithOwner, Kind: q.kind,
					Number: h.Number, Author: h.Author.Login, UpdatedAt: h.UpdatedAt, IsDraft: h.IsDraft,
				}
				byURL[h.URL] = item
				via[h.URL] = acct.GH
				*order = append(*order, h.URL)
			}
			reasons := []string{q.reason}
			if q.reason == "" {
				reasons = involvement(h, acct.Login)
			}
			for _, r := range reasons {
				if !slices.Contains(item.Reasons, r) {
					item.Reasons = append(item.Reasons, r)
				}
			}
		}
	}
	return nil
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

func search(ctx context.Context, gh ghRunner, q query) ([]hit, error) {
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
	out, err := gh(ctx, args...)
	if err != nil {
		return nil, err
	}
	var hits []hit
	if err := json.Unmarshal(out, &hits); err != nil {
		return nil, err
	}
	return hits, nil
}
