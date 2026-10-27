package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
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
		newDocsAddCmd(), newDocsStatusCmd(), newDocsSyncCmd(cf),
		newDocsRefreshCmd(cf), newDocsListCmd(), newDocsOwnersCmd(), newDocsRemoveCmd(),
	)
	return cmd
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

func newDocsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List ingested documents and how long since each was last ingested",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
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

func newDocsOwnersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "owners",
		Short: "Inspect registered docs owners",
	}
	cmd.AddCommand(newDocsOwnersListCmd())
	return cmd
}

func newDocsOwnersListCmd() *cobra.Command {
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
			if err != nil {
				return err
			}
			if len(owners) == 0 {
				fmt.Fprintln(c.OutOrStdout(), "no docs owners registered")
				return nil
			}
			for _, o := range owners {
				fmt.Fprintf(c.OutOrStdout(), "%-20s status=%s refresh=%ds\n", o.Owner, o.Status, o.RefreshSecs)
			}
			return nil
		},
	}
}

func newDocsStatusCmd() *cobra.Command {
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
	var background bool
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
			return runDocsSync(c, cf, reg, ownerArg(args), false, background)
		},
	}
	cmd.Flags().BoolVar(&background, "background", false, "start the sync and return without following it")
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
	out := c.OutOrStdout()
	client, cleanup, err := cf()
	if err != nil {
		return docsSyncDeferred(c, reg, owner, force, err)
	}
	defer cleanup()
	callCtx, cancel := context.WithTimeout(c.Context(), 45*time.Second)
	requested := time.Now().Add(-time.Second)
	_, err = client.SyncDocs(callCtx, &orchdv1.SyncDocsRequest{Owner: owner, Force: force})
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
