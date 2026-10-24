package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/github"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func newGitHubCmd(cf clientFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "github",
		Short: "Choose which GitHub accounts and orgs orch monitors",
		Long: `orch monitors your own GitHub account by default. Each org you belong to is off
until you enable it; an enabled org covers every repo you can see, narrowed by
--include or --exclude. orchd talks to GitHub through gh.`,
	}
	cmd.AddCommand(
		newGitHubScopesCmd(cf),
		operatorOnlyMsg("queue tasks cannot change GitHub monitoring", newGitHubEnableCmd(cf)),
		operatorOnlyMsg("queue tasks cannot change GitHub monitoring", newGitHubDisableCmd(cf)),
		newGitHubTeamsCmd(cf),
		newGitHubReposCmd(cf),
	)
	return cmd
}

// withOrchd dials orchd and runs fn with a deadline that covers gh calls.
func withOrchd(cf clientFactory, fn func(context.Context, orchdv1.OrchdClient) error) error {
	client, cleanup, err := cf()
	if err != nil {
		return err
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return fn(ctx, client)
}

func newGitHubScopesCmd(cf clientFactory) *cobra.Command {
	var refresh, asJSON bool
	cmd := &cobra.Command{
		Use:   "scopes",
		Short: "List your account and orgs, and whether each is monitored",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return withOrchd(cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
				resp, err := client.ListGitHubScopes(ctx, &orchdv1.ListGitHubScopesRequest{Refresh: refresh})
				if err != nil {
					return err
				}
				if asJSON {
					return json.NewEncoder(c.OutOrStdout()).Encode(resp.GetScopes())
				}
				printScopes(c.OutOrStdout(), resp)
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&refresh, "refresh", false, "rediscover your orgs now")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}

func printScopes(w io.Writer, resp *orchdv1.ListGitHubScopesResponse) {
	if warn := resp.GetWarning(); warn != "" {
		fmt.Fprintln(w, "warning:", warn)
	}
	if len(resp.GetScopes()) == 0 {
		fmt.Fprintln(w, "no scopes yet (orchd could not discover your GitHub account)")
		return
	}
	for _, s := range resp.GetScopes() {
		state := "off"
		if s.GetEnabled() {
			state = "on"
		}
		fmt.Fprintf(w, "%-3s %-28s %-4s items=%-9s %s\n", state, s.GetOwner(), s.GetKind(), s.GetItems(), describeRepos(s))
	}
}

func describeRepos(s *orchdv1.GitHubScope) string {
	switch s.GetRepoMode() {
	case github.ReposInclude:
		return "repos: only " + strings.Join(s.GetRepos(), ", ")
	case github.ReposExclude:
		return "repos: all but " + strings.Join(s.GetRepos(), ", ")
	default:
		return "repos: all"
	}
}

// findScope returns the stored scope for owner, so a change keeps the rest.
func findScope(ctx context.Context, client orchdv1.OrchdClient, owner string) (*orchdv1.GitHubScope, error) {
	resp, err := client.ListGitHubScopes(ctx, &orchdv1.ListGitHubScopesRequest{})
	if err != nil {
		return nil, err
	}
	for _, s := range resp.GetScopes() {
		if strings.EqualFold(s.GetOwner(), owner) {
			return s, nil
		}
	}
	return nil, fmt.Errorf("%q is not your account or an org you belong to (see: orch github scopes --refresh)", owner)
}

func newGitHubEnableCmd(cf clientFactory) *cobra.Command {
	var allItems bool
	var include, exclude []string
	cmd := &cobra.Command{
		Use:   "enable <owner>",
		Short: "Monitor an account or org (all repos you can see, unless narrowed)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return withOrchd(cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
				scope, err := findScope(ctx, client, args[0])
				if err != nil {
					return err
				}
				scope.Enabled = true
				scope.Items = github.ItemsInvolving
				if allItems {
					scope.Items = github.ItemsAll
				}
				scope.RepoMode, scope.Repos = github.ReposAll, nil
				switch {
				case len(include) > 0:
					scope.RepoMode, scope.Repos = github.ReposInclude, include
				case len(exclude) > 0:
					scope.RepoMode, scope.Repos = github.ReposExclude, exclude
				}
				resp, err := client.SetGitHubScope(ctx, &orchdv1.SetGitHubScopeRequest{Scope: scope})
				if err != nil {
					return err
				}
				s := resp.GetScope()
				fmt.Fprintf(c.OutOrStdout(), "monitoring %s (items=%s, %s)\n", s.GetOwner(), s.GetItems(), describeRepos(s))
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&allItems, "all-items", false, "every open PR and issue, not only those involving you")
	cmd.Flags().StringSliceVar(&include, "include", nil, "only these repos (comma-separated names)")
	cmd.Flags().StringSliceVar(&exclude, "exclude", nil, "every repo but these (comma-separated names)")
	cmd.MarkFlagsMutuallyExclusive("include", "exclude")
	return cmd
}

func newGitHubDisableCmd(cf clientFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "disable <owner>",
		Short: "Stop monitoring an account or org (its settings are kept)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return withOrchd(cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
				scope, err := findScope(ctx, client, args[0])
				if err != nil {
					return err
				}
				scope.Enabled = false
				resp, err := client.SetGitHubScope(ctx, &orchdv1.SetGitHubScopeRequest{Scope: scope})
				if err != nil {
					return err
				}
				fmt.Fprintf(c.OutOrStdout(), "stopped monitoring %s\n", resp.GetScope().GetOwner())
				return nil
			})
		},
	}
}

func newGitHubTeamsCmd(cf clientFactory) *cobra.Command {
	var refresh bool
	cmd := &cobra.Command{
		Use:   "teams",
		Short: "List the GitHub teams you are on",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return withOrchd(cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
				id, err := client.GetGitHubIdentity(ctx, &orchdv1.GetGitHubIdentityRequest{Refresh: refresh})
				if err != nil {
					return err
				}
				if len(id.GetTeams()) == 0 {
					fmt.Fprintf(c.OutOrStdout(), "%s is on no teams\n", id.GetLogin())
					return nil
				}
				for _, t := range id.GetTeams() {
					fmt.Fprintf(c.OutOrStdout(), "%s/%s  %s\n", t.GetOrg(), t.GetSlug(), github.Printable(t.GetName(), 200))
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&refresh, "refresh", false, "ask GitHub now rather than the 10-minute cache")
	return cmd
}

func newGitHubReposCmd(cf clientFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "repos <owner>",
		Short: "List the repos you can see under an account or org",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return withOrchd(cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
				resp, err := client.ListGitHubRepos(ctx, &orchdv1.ListGitHubReposRequest{Owner: args[0]})
				if err != nil {
					return err
				}
				for _, r := range resp.GetRepos() {
					var notes []string
					if r.GetPrivate() {
						notes = append(notes, "private")
					}
					if r.GetArchived() {
						notes = append(notes, "archived")
					}
					fmt.Fprintf(c.OutOrStdout(), "%s %s\n", r.GetName(), strings.Join(notes, " "))
				}
				return nil
			})
		},
	}
}
