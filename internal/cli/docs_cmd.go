package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/docs"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func newDocsCmd(cf clientFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "docs",
		Short: "Ingest GitHub code-repo docs (markdown) for an owner on a cadence",
	}
	cmd.AddCommand(
		docsOperatorOnly(newDocsAddCmd()), newDocsStatusCmd(cf), newDocsSyncCmd(cf),
		newDocsRefreshCmd(cf), newDocsListCmd(cf), newDocsOwnersCmd(cf), docsOperatorOnly(newDocsRemoveCmd()),
		docsOperatorOnly(newDocsToggleCmd(cf, "enable", true)), docsOperatorOnly(newDocsToggleCmd(cf, "disable", false)),
		docsOperatorOnly(newDocsReposCmd(cf)), newDocsPurgeCmd(cf),
	)
	return cmd
}

// docsOperatorOnly refuses a command that changes what is synced inside a task.
func docsOperatorOnly(cmd *cobra.Command) *cobra.Command {
	return operatorOnlyMsg("queue tasks cannot change which docs are synced; report it in the task result instead", cmd)
}

func openDocsRegistry() (*docs.Registry, func(), error) {
	conn, err := db.Open()
	if err != nil {
		return nil, nil, err
	}
	return docs.NewRegistry(conn), func() { _ = conn.Close() }, nil
}

func newDocsAddCmd() *cobra.Command {
	var (
		refresh string
		scope   string
		tags    []string
	)
	cmd := &cobra.Command{
		Use:   "add <owner>",
		Short: "Register a GitHub owner (user/org) for markdown ingestion",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			d, err := time.ParseDuration(refresh)
			if err != nil {
				return fmt.Errorf("invalid --refresh: %w", err)
			}
			reg, closeFn, err := openDocsRegistry()
			if err != nil {
				return err
			}
			defer closeFn()
			if err := reg.AddOwner(c.Context(), docs.Owner{
				Owner: args[0], Scope: scope, Tags: tags, RefreshSecs: int64(d.Seconds()),
			}); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "added docs owner %s (refresh %s)\n", args[0], refresh)
			return nil
		},
	}
	cmd.Flags().StringVar(&refresh, "refresh", "6h", "refresh cadence (e.g. 6h, 24h)")
	cmd.Flags().StringVar(&scope, "scope", "", "scope tag")
	cmd.Flags().StringSliceVar(&tags, "tag", nil, "tag (repeatable)")
	return cmd
}

func newDocsListCmd(cf clientFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List ingested documents and how long since each was last ingested",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if policyApplied(c.Context()) {
				return printDocsFromOrchd(c, orchdLists{cf: cf})
			}
			reg, closeFn, err := openDocsRegistry()
			if err != nil {
				return err
			}
			defer closeFn()
			docList, err := reg.ListDocs(c.Context())
			if err != nil {
				return err
			}
			if len(docList) == 0 {
				fmt.Fprintln(c.OutOrStdout(), "no docs ingested")
				return nil
			}
			for _, d := range docList {
				fmt.Fprintf(c.OutOrStdout(), "%-10s %s\n", humanAgo(d.UpdatedAt), d.URI)
			}
			return nil
		},
	}
}

// printDocsFromOrchd prints the synced docs orchd lists for the caller.
func printDocsFromOrchd(c *cobra.Command, l orchdLists) error {
	rep, err := l.docs(c.Context())
	if err != nil {
		return err
	}
	if len(rep.GetFiles()) == 0 {
		fmt.Fprintln(c.OutOrStdout(), "no docs ingested")
		return nil
	}
	for _, f := range rep.GetFiles() {
		fmt.Fprintf(c.OutOrStdout(), "%-10s %s\n", humanAgo(f.GetUpdatedAt()), f.GetSourceUri())
	}
	if rep.GetTruncated() {
		fmt.Fprintln(c.OutOrStdout(), "(more docs than one reply carries; not all are shown)")
	}
	return nil
}

// docOwnersShown keeps of owners those the caller may see: under a spaces
// policy the ones orchd lists for it, else all.
func docOwnersShown(c *cobra.Command, cf clientFactory, owners []docs.Owner) ([]docs.Owner, error) {
	if !policyApplied(c.Context()) {
		return owners, nil
	}
	shown, err := orchdLists{cf: cf}.docOwners(c.Context())
	if err != nil {
		return nil, err
	}
	out := owners[:0:0]
	for _, o := range owners {
		if shown[o.Owner] {
			out = append(out, o)
		}
	}
	return out, nil
}

func newDocsOwnersCmd(cf clientFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "owners",
		Short: "Inspect registered docs owners",
	}
	cmd.AddCommand(newDocsOwnersListCmd(cf))
	return cmd
}

func newDocsOwnersListCmd(cf clientFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered docs owners",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			reg, closeFn, err := openDocsRegistry()
			if err != nil {
				return err
			}
			defer closeFn()
			owners, err := reg.ListOwners(c.Context())
			if err == nil {
				owners, err = docOwnersShown(c, cf, owners)
			}
			if err != nil {
				return err
			}
			if len(owners) == 0 {
				fmt.Fprintln(c.OutOrStdout(), "no docs owners registered")
				return nil
			}
			for _, o := range owners {
				fmt.Fprintf(c.OutOrStdout(), "%-20s status=%s refresh=%ds covers=%s\n", o.Owner, o.Status, o.RefreshSecs, ownerCovers(o))
			}
			return nil
		},
	}
}

func newDocsStatusCmd(cf clientFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show per-owner sync status (last run, next due, repos, files)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			reg, closeFn, err := openDocsRegistry()
			if err != nil {
				return err
			}
			defer closeFn()
			owners, err := reg.ListOwners(c.Context())
			if err == nil {
				owners, err = docOwnersShown(c, cf, owners)
			}
			if err != nil {
				return err
			}
			if len(owners) == 0 {
				fmt.Fprintln(c.OutOrStdout(), "no docs owners registered")
				return nil
			}
			for _, o := range owners {
				repos, _ := reg.ListRepos(c.Context(), o.Owner)
				files := 0
				for _, r := range repos {
					files += r.Files
				}
				fmt.Fprintf(c.OutOrStdout(), "%-20s last=%s  next=%s  repos=%d files=%d status=%s\n",
					o.Owner, humanAgo(o.LastSynced), nextDue(o.LastSynced, o.RefreshSecs), len(repos), files, o.Status)
			}
			return nil
		},
	}
}

func newDocsSyncCmd(cf clientFactory) *cobra.Command {
	var background, confirmPrune bool
	cmd := &cobra.Command{
		Use:   "sync [owner]",
		Short: "Ask orchd to sync now (skips unchanged repos + files) and follow it",
		Long: `Ask orchd to sync docs now and show its progress. orchd runs one docs sync
at a time: if one is already running, this says so and exits (nothing is queued).`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			reg, closeFn, err := openDocsRegistry()
			if err != nil {
				return err
			}
			defer closeFn()
			if confirmPrune && ownerArg(args) == "" {
				return errors.New("--confirm-prune needs an owner")
			}
			return runDocsSyncOpts(c, cf, reg, &orchdv1.SyncDocsRequest{Owner: ownerArg(args), ConfirmPrune: confirmPrune}, background)
		},
	}
	cmd.Flags().BoolVar(&background, "background", false, "start the sync and return without following it")
	cmd.Flags().BoolVar(&confirmPrune, "confirm-prune", false, "let this sync remove more docs gone from a repo than the prune cap allows")
	return cmd
}

func newDocsRefreshCmd(cf clientFactory) *cobra.Command {
	var background bool
	cmd := &cobra.Command{
		Use:   "refresh [owner]",
		Short: "Ask orchd for a full re-ingest (clears repo-SHA + file-hash caches) and follow it",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			reg, closeFn, err := openDocsRegistry()
			if err != nil {
				return err
			}
			defer closeFn()
			return runDocsSync(c, cf, reg, ownerArg(args), true, background)
		},
	}
	cmd.Flags().BoolVar(&background, "background", false, "start the re-ingest and return without following it")
	return cmd
}

func ownerArg(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	return ""
}

// docsPollInterval is how often a followed sync is checked; a variable so
// tests need not wait.
var docsPollInterval = 2 * time.Second

// docsStartWait bounds how long to wait for orchd to begin a requested sync
// before leaving it to orchd.
var docsStartWait = 30 * time.Second

// runDocsSync asks orchd to sync, so every sync runs in orchd, one at a time.
// It follows the sync unless background is set, then shows how each owner
// fared. If orchd is unreachable, the owner is marked due (and with force its
// caches cleared) so orchd syncs on its next scan.
func runDocsSync(c *cobra.Command, cf clientFactory, reg *docs.Registry, owner string, force, background bool) error {
	return runDocsSyncOpts(c, cf, reg, &orchdv1.SyncDocsRequest{Owner: owner, Force: force}, background)
}

func runDocsSyncOpts(c *cobra.Command, cf clientFactory, reg *docs.Registry, req *orchdv1.SyncDocsRequest, background bool) error {
	owner, force := req.GetOwner(), req.GetForce()
	out := c.OutOrStdout()
	client, cleanup, err := cf()
	if err != nil {
		return docsSyncDeferred(c, reg, owner, force, err)
	}
	defer cleanup()
	callCtx, cancel := context.WithTimeout(c.Context(), 45*time.Second)
	requested := time.Now().Add(-time.Second)
	_, err = client.SyncDocs(callCtx, req)
	cancel()
	switch grpcstatus.Code(err) {
	case codes.OK:
	case codes.Unavailable:
		// orchd is not reachable. A deadline is not treated the same way:
		// orchd may have accepted the request, and clearing caches here
		// would race its sync.
		return docsSyncDeferred(c, reg, owner, force, err)
	default:
		return errors.New(grpcstatus.Convert(err).Message())
	}
	if background {
		fmt.Fprintln(out, "orchd is syncing; follow it with: orch docs status")
		return nil
	}
	if err := followDocsSync(c.Context(), client, out, requested); err != nil {
		return err
	}
	owners, err := selectOwners(c.Context(), reg, owner)
	if err != nil {
		return err
	}
	var failed bool
	for _, o := range owners {
		failed = printOwnerResult(c.Context(), out, reg, o) || failed
	}
	if failed {
		return errors.New("the docs sync needs attention: see the errors above")
	}
	return nil
}

// followDocsSync prints the sync's progress until it finishes.
func followDocsSync(ctx context.Context, client orchdv1.OrchdClient, out io.Writer, requested time.Time) error {
	began := false
	lastDone := -1
	wait := time.Now().Add(docsStartWait)
	for {
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		st, err := client.GetDocsSync(callCtx, &orchdv1.GetDocsSyncRequest{})
		cancel()
		if err != nil {
			return err
		}
		started := st.GetStartedAt() >= requested.Unix()
		if st.GetRunning() && started {
			began = true
			// Nothing to show until orchd has listed the owner's repos.
			if st.GetTotal() > 0 && int(st.GetDone()) != lastDone {
				lastDone = int(st.GetDone())
				fmt.Fprintf(out, "%s: %d/%d repos (last: %s)\n", st.GetOwner(), st.GetDone(), st.GetTotal(), st.GetRepo())
			}
		} else if began || (started && st.GetFinishedAt() > 0) {
			return nil
		} else if time.Now().After(wait) {
			fmt.Fprintln(out, "orchd has not started the sync yet; it will run on its next scan")
			return nil
		}
		select {
		case <-ctx.Done():
			fmt.Fprintln(out, "stopped following; orchd keeps syncing")
			return nil
		case <-time.After(docsPollInterval):
		}
	}
}

// docsSyncDeferred is the daemon-unreachable fallback: mark due (clearing the
// caches for a re-ingest) so the sync still happens once orchd is back.
func docsSyncDeferred(c *cobra.Command, reg *docs.Registry, owner string, force bool, cause error) error {
	if force {
		if err := reg.ClearCaches(c.Context(), owner); err != nil {
			return err
		}
	}
	if err := reg.MarkDue(c.Context(), owner); err != nil {
		return err
	}
	fmt.Fprintf(c.OutOrStdout(),
		"orchd unreachable (%v); marked for sync — orchd will sync on its next scan\n", cause)
	return nil
}

// selectOwners resolves the optional owner argument to the owners to sync.
func selectOwners(ctx context.Context, reg *docs.Registry, owner string) ([]docs.Owner, error) {
	all, err := reg.ListOwners(ctx)
	if err != nil {
		return nil, err
	}
	if owner == "" {
		return all, nil
	}
	for _, o := range all {
		if o.Owner == owner {
			return []docs.Owner{o}, nil
		}
	}
	return nil, fmt.Errorf("docs owner %q is not registered", owner)
}

// printOwnerResult shows how an owner's last sync went, with each failing
// repo and why; it reports whether anything needs attention.
func printOwnerResult(ctx context.Context, out io.Writer, reg *docs.Registry, o docs.Owner) bool {
	repos, _ := reg.ListRepos(ctx, o.Owner)
	files := 0
	var failing []docs.RepoState
	for _, r := range repos {
		files += r.Files
		if r.Status == "error" {
			failing = append(failing, r)
		}
	}
	fmt.Fprintf(out, "%-20s repos=%d files=%d status=%s\n", o.Owner, len(repos), files, o.Status)
	if o.Error != "" {
		fmt.Fprintf(out, "  %s\n", o.Error)
	}
	for i, r := range failing {
		if i == 10 {
			fmt.Fprintf(out, "  … and %d more failing repos\n", len(failing)-10)
			break
		}
		fmt.Fprintf(out, "  %s: %s\n", r.Repo, r.Error)
	}
	return o.Error != "" || len(failing) > 0
}

func newDocsRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <owner>",
		Short: "Remove a docs owner and its sync caches",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			reg, closeFn, err := openDocsRegistry()
			if err != nil {
				return err
			}
			defer closeFn()
			if err := reg.RemoveOwner(c.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "removed docs owner %s\n", args[0])
			return nil
		},
	}
}

// ownerCovers says in a few words what an owner syncs.
func ownerCovers(o docs.Owner) string {
	switch {
	case !o.Enabled:
		return "off"
	case o.RepoMode == docs.ReposInclude:
		return "only:" + strings.Join(o.Repos, ",")
	case o.RepoMode == docs.ReposExclude:
		return "except:" + strings.Join(o.Repos, ",")
	}
	return "all"
}

// findOwner loads a registered owner.
func findOwner(c *cobra.Command, reg *docs.Registry, owner string) (docs.Owner, error) {
	owners, err := selectOwners(c.Context(), reg, owner)
	if err != nil {
		return docs.Owner{}, err
	}
	return owners[0], nil
}

// saveOwnerChoice goes through orchd, whose guard refuses a queued task and which
// refuses the change while a sync runs, since the sync would overwrite it. With
// orchd down no sync can run, so the registry is written directly.
func saveOwnerChoice(c *cobra.Command, cf clientFactory, reg *docs.Registry, o docs.Owner, choice docs.Choice) (docs.Owner, error) {
	client, cleanup, err := cf()
	if err == nil {
		defer cleanup()
		_, err = client.SaveDocsOwner(c.Context(), &orchdv1.SaveDocsOwnerRequest{
			Owner: o.Owner, Enabled: choice.Enabled, RepoMode: choice.RepoMode, Repos: choice.Repos,
		})
		if err == nil {
			return findOwner(c, reg, o.Owner)
		}
		if grpcstatus.Code(err) != codes.Unavailable {
			return docs.Owner{}, err
		}
	}
	return reg.SetChoice(c.Context(), o.Owner, choice)
}

func newDocsToggleCmd(cf clientFactory, verb string, enabled bool) *cobra.Command {
	return &cobra.Command{
		Use:   verb + " <owner>",
		Short: "Turn a docs owner's sync on or off; its indexed docs stay either way",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			reg, closeFn, err := openDocsRegistry()
			if err != nil {
				return err
			}
			defer closeFn()
			o, err := findOwner(c, reg, args[0])
			if err != nil {
				return err
			}
			next, err := saveOwnerChoice(c, cf, reg, o, docs.Choice{Enabled: enabled, RepoMode: ownerMode(o), Repos: o.Repos})
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "%s covers %s\n", next.Owner, ownerCovers(next))
			return nil
		},
	}
}

func ownerMode(o docs.Owner) string {
	if o.RepoMode == "" {
		return docs.ReposAll
	}
	return o.RepoMode
}

func newDocsReposCmd(cf clientFactory) *cobra.Command {
	var all bool
	var only, except []string
	cmd := &cobra.Command{
		Use:   "repos <owner>",
		Short: "Choose which of a docs owner's repos are synced; opted-out repos keep their indexed docs",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			choice := docs.Choice{RepoMode: docs.ReposAll}
			switch {
			case all && len(only) == 0 && len(except) == 0:
			case !all && len(only) > 0 && len(except) == 0:
				choice.RepoMode, choice.Repos = docs.ReposInclude, only
			case !all && len(except) > 0 && len(only) == 0:
				choice.RepoMode, choice.Repos = docs.ReposExclude, except
			default:
				return fmt.Errorf("give exactly one of --all, --only or --except")
			}
			reg, closeFn, err := openDocsRegistry()
			if err != nil {
				return err
			}
			defer closeFn()
			o, err := findOwner(c, reg, args[0])
			if err != nil {
				return err
			}
			choice.Enabled = o.Enabled
			next, err := saveOwnerChoice(c, cf, reg, o, choice)
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "%s covers %s\n", next.Owner, ownerCovers(next))
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "sync every repo")
	cmd.Flags().StringSliceVar(&only, "only", nil, "sync only these repos")
	cmd.Flags().StringSliceVar(&except, "except", nil, "sync every repo but these")
	return cmd
}

func newDocsPurgeCmd(cf clientFactory) *cobra.Command {
	var confirm bool
	cmd := &cobra.Command{
		Use:   "purge <owner> [repo...]",
		Short: "Delete the indexed docs of repos an owner no longer covers (permanent)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if !confirm {
				return fmt.Errorf("this permanently deletes indexed docs; pass --confirm to go ahead")
			}
			client, cleanup, err := cf()
			if err != nil {
				return err
			}
			defer cleanup()
			resp, err := client.PurgeDocs(c.Context(), &orchdv1.PurgeDocsRequest{Owner: args[0], Repos: args[1:], Confirm: true})
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "deleted %d docs (%d chunks)\n", resp.GetPurgedSources(), resp.GetPurgedChunks())
			return nil
		},
	}
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm the permanent deletion")
	return cmd
}

// humanAgo renders a unix timestamp as a coarse "N ago" string ("never" if 0).
func humanAgo(unix int64) string {
	if unix == 0 {
		return "never"
	}
	d := time.Since(time.Unix(unix, 0))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// nextDue renders when an owner next becomes due ("now" if already due/never).
func nextDue(lastSynced, refreshSecs int64) string {
	if lastSynced == 0 {
		return "now"
	}
	due := time.Unix(lastSynced+refreshSecs, 0)
	rem := time.Until(due)
	if rem <= 0 {
		return "now"
	}
	if rem < time.Hour {
		return fmt.Sprintf("in %dm", int(rem.Minutes()))
	}
	return fmt.Sprintf("in %dh", int(rem.Hours()))
}
