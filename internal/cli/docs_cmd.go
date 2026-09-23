package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/docs"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// Seams for the foreground sync's external tooling, overridable in tests.
var (
	newDocsGitHub = docs.NewGitHub
	newDocsGit    = docs.NewGit
)

func newDocsCmd(cf clientFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "docs",
		Short: "Ingest GitHub code-repo docs (markdown) for an owner on a cadence",
	}
	cmd.AddCommand(
		newDocsAddCmd(), newDocsStatusCmd(), newDocsSyncCmd(cf),
		newDocsRefreshCmd(cf), newDocsListCmd(), newDocsRemoveCmd(),
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
		Short: "Run an incremental sync now (skips unchanged repos + files)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			reg, closeFn, err := openDocsRegistry()
			if err != nil {
				return err
			}
			defer closeFn()
			return runDocsSync(c, cf, reg, ownerArg(args), background)
		},
	}
	cmd.Flags().BoolVar(&background, "background", false, "only mark due; let orchd sync on its next scan")
	return cmd
}

func newDocsRefreshCmd(cf clientFactory) *cobra.Command {
	var background bool
	cmd := &cobra.Command{
		Use:   "refresh [owner]",
		Short: "Force a full re-ingest (clears repo-SHA + file-hash caches), then sync now",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			reg, closeFn, err := openDocsRegistry()
			if err != nil {
				return err
			}
			defer closeFn()
			owner := ownerArg(args)
			if err := reg.ClearCaches(c.Context(), owner); err != nil {
				return err
			}
			fmt.Fprintln(c.OutOrStdout(), "caches cleared")
			return runDocsSync(c, cf, reg, owner, background)
		},
	}
	cmd.Flags().BoolVar(&background, "background", false, "only mark due; let orchd re-ingest on its next scan")
	return cmd
}

func ownerArg(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	return ""
}

// runDocsSync syncs in the foreground so the user sees the result of this
// command rather than waiting on orchd's scan. Embedding + storage still belong
// to the daemon, so ingestion is routed over its Ingest RPC; if the daemon is
// unreachable (or --background is set) we fall back to marking the owner due.
func runDocsSync(c *cobra.Command, cf clientFactory, reg *docs.Registry, owner string, background bool) error {
	out := c.OutOrStdout()
	if background {
		if err := reg.MarkDue(c.Context(), owner); err != nil {
			return err
		}
		fmt.Fprintln(out, "marked for sync; orchd will sync on its next scan")
		return nil
	}

	client, cleanup, err := cf()
	if err != nil {
		return docsSyncDeferred(c, reg, owner, err)
	}
	defer cleanup()
	pingCtx, cancel := context.WithTimeout(c.Context(), 3*time.Second)
	defer cancel()
	if _, err := client.Ping(pingCtx, &orchdv1.PingRequest{}); err != nil {
		return docsSyncDeferred(c, reg, owner, err)
	}

	owners, err := selectOwners(c.Context(), reg, owner)
	if err != nil {
		return err
	}
	if len(owners) == 0 {
		fmt.Fprintln(out, "no docs owners registered")
		return nil
	}

	s, err := loadSettings()
	if err != nil {
		return err
	}
	opts := docs.SyncOpts{
		RepoCache:       s.RepoCacheDir(),
		IncludeArchived: s.Docs.IncludeArchived,
		IncludeForks:    s.Docs.IncludeForks,
		GH:              newDocsGitHub(),
		Git:             newDocsGit(),
		Now:             func() int64 { return time.Now().Unix() },
	}
	ing := rpcIngester{client: client}

	var firstErr error
	for _, o := range owners {
		fmt.Fprintf(out, "syncing %s ...\n", o.Owner)
		status, err := docs.SyncOwner(c.Context(), reg, o, ing, opts)
		if err := reg.MarkSynced(c.Context(), o.Owner, time.Now().Unix(), status); err != nil && firstErr == nil {
			firstErr = err
		}
		if err != nil {
			fmt.Fprintf(out, "%-20s status=error: %v\n", o.Owner, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		printOwnerResult(c.Context(), out, reg, o.Owner, status)
	}
	return firstErr
}

// docsSyncDeferred is the daemon-unreachable fallback: mark due so the sync
// still happens once orchd is back, and say why it didn't run now.
func docsSyncDeferred(c *cobra.Command, reg *docs.Registry, owner string, cause error) error {
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

func printOwnerResult(ctx context.Context, out io.Writer, reg *docs.Registry, owner, status string) {
	repos, _ := reg.ListRepos(ctx, owner)
	files := 0
	for _, r := range repos {
		files += r.Files
	}
	fmt.Fprintf(out, "%-20s synced repos=%d files=%d status=%s\n", owner, len(repos), files, status)
}

// rpcIngester adapts orchd's Ingest RPC to the docs.Ingester seam so a CLI-side
// sync reuses the daemon's embedder and vector store instead of loading its own.
type rpcIngester struct{ client orchdv1.OrchdClient }

func (r rpcIngester) IngestText(ctx context.Context, text string, m rag.Meta) (int, error) {
	resp, err := r.client.Ingest(ctx, &orchdv1.IngestRequest{
		Text:       text,
		SourceUri:  m.SourceURI,
		SourceType: m.SourceType,
		Scope:      m.Scope,
		Tags:       m.Tags,
	})
	if err != nil {
		return 0, err
	}
	return int(resp.GetChunks()), nil
}

// loadSettings resolves config for the foreground sync (defaults < file < env).
func loadSettings() (config.Settings, error) {
	v, err := config.Load()
	if err != nil {
		return config.Settings{}, err
	}
	return config.Unmarshal(v)
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
