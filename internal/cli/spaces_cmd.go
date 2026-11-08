package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/github"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// newSpacesCmd drafts, reviews and applies the spaces policy, which orchd
// keeps in orch.db. Saving a draft changes nothing; only an apply from outside
// any agent session does.
func newSpacesCmd(cf clientFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "spaces",
		Short: "Draft, review and apply the spaces policy",
		Long: `The spaces policy says which GitHub orgs, folder roots, web prefixes and
chat tenants belong to which space, and which spaces may read which. orchd
stores it in orch.db as numbered revisions and enforces only the one the
operator applied.

  orch spaces draft < policy.json    save a draft (changes nothing)
  orch spaces diff                   compare the draft with what is applied
  orch spaces apply --revision N     enforce draft N (operator only)

Diff and apply are refused inside an agent session (Claude Code, Codex) and
inside tasks orchd runs: run them in your own terminal or use Orch.app. An
agent session may save a draft; applying one needs --accept-agent-draft.`,
	}
	cmd.AddCommand(newSpacesDraftCmd(cf), newSpacesDiffCmd(cf), newSpacesApplyCmd(cf))
	return cmd
}

func newSpacesDraftCmd(cf clientFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "draft",
		Short: "Save a spaces policy (JSON on stdin) as a new draft revision",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			// One byte over orchd's limit, so an oversized policy is refused
			// by orchd with its message rather than cut short here.
			doc, err := io.ReadAll(io.LimitReader(c.InOrStdin(), 1<<20+1))
			if err != nil {
				return err
			}
			return withClient(cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
				d, err := client.SetSpacesDraft(ctx, &orchdv1.SetSpacesDraftRequest{PolicyJson: string(doc)})
				if err != nil {
					return err
				}
				out := c.OutOrStdout()
				fmt.Fprintf(out, "saved draft revision %d\n", d.GetRevision())
				printProblems(out, d.GetProblems())
				if len(d.GetProblems()) == 0 {
					fmt.Fprintf(out, "review it with 'orch spaces diff', then 'orch spaces apply --revision %d'\n", d.GetRevision())
				}
				return nil
			})
		},
	}
}

func newSpacesDiffCmd(cf clientFactory) *cobra.Command {
	var full bool
	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Compare the newest draft with the applied spaces policy",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return withClient(cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
				d, err := client.DiffSpaces(ctx, &orchdv1.DiffSpacesRequest{})
				if err != nil {
					return err
				}
				out := c.OutOrStdout()
				applied := "none"
				if n := d.GetAppliedRevision(); n > 0 {
					applied = fmt.Sprintf("revision %d", n)
				}
				fmt.Fprintf(out, "applied: %s\n", applied)
				if orgs := d.GetUnconfirmedGithubOrgs(); len(orgs) > 0 {
					shown := make([]string, len(orgs))
					for i, org := range orgs {
						shown[i] = github.Printable(org, 64)
					}
					fmt.Fprintf(out, "not confirmed: GitHub org(s) %s; their new content goes to quarantine\n", strings.Join(shown, ", "))
					fmt.Fprintln(out, "  an org's GitHub identity changed, or was never recorded; if you expect that, apply the newest revision with --confirm-github-org <org>")
				}
				if d.GetDraftRevision() == 0 {
					fmt.Fprintln(out, "no draft newer than the applied policy")
					return nil
				}
				by := "you"
				if !d.GetDraftByOperator() {
					// Reason: an agent may propose a draft; the reviewer must
					// see that it was not theirs before applying it.
					by = "an agent session (" + d.GetDraftCreatedBy() + "); review every value below"
				}
				fmt.Fprintf(out, "draft:   revision %d, saved %s by %s\n", d.GetDraftRevision(),
					time.Unix(d.GetDraftCreatedAt(), 0).Local().Format("2006-01-02 15:04"), by)
				if len(d.GetChanges()) == 0 {
					fmt.Fprintln(out, "no changes")
				}
				for _, ch := range d.GetChanges() {
					name := ch.GetSpace()
					if name == "" {
						name = "(policy)"
					}
					line := fmt.Sprintf("  %-7s %s", ch.GetKind(), name)
					if len(ch.GetFields()) > 0 {
						line += ": " + strings.Join(ch.GetFields(), ", ")
					}
					fmt.Fprintln(out, line)
					for _, detail := range ch.GetDetails() {
						fmt.Fprintf(out, "      %s\n", detail)
					}
				}
				printProblems(out, d.GetProblems())
				if full {
					fmt.Fprintf(out, "\napplied policy:\n%s\n\ndraft policy:\n%s\n", orNone(d.GetAppliedJson()), d.GetDraftJson())
				}
				if len(d.GetProblems()) == 0 {
					flag := ""
					if !d.GetDraftByOperator() {
						flag = " --accept-agent-draft"
					}
					fmt.Fprintf(out, "to enforce it: orch spaces apply --revision %d%s\n", d.GetDraftRevision(), flag)
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&full, "full", false, "also print both policy documents")
	return cmd
}

func newSpacesApplyCmd(cf clientFactory) *cobra.Command {
	var revision int64
	var acceptAgentDraft bool
	var confirmOrgs []string
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Enforce the draft revision you reviewed (operator only)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if revision <= 0 {
				return fmt.Errorf("name the revision you reviewed: orch spaces apply --revision N (see orch spaces diff)")
			}
			return withClient(cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
				resp, err := client.ApplySpaces(ctx, &orchdv1.ApplySpacesRequest{
					ReviewedRevision: revision, AcceptAgentDraft: acceptAgentDraft, ConfirmGithubOrgs: confirmOrgs,
				})
				if err != nil {
					return err
				}
				fmt.Fprintf(c.OutOrStdout(), "applied spaces revision %d\n", resp.GetAppliedRevision())
				return nil
			})
		},
	}
	cmd.Flags().Int64Var(&revision, "revision", 0, "the draft revision you reviewed")
	cmd.Flags().BoolVar(&acceptAgentDraft, "accept-agent-draft", false,
		"apply a draft an agent session saved, after reviewing every value in 'orch spaces diff'")
	cmd.Flags().StringSliceVar(&confirmOrgs, "confirm-github-org", nil,
		"accept the changed GitHub identity of this bound org (repeatable), after checking it on GitHub")
	return cmd
}

func withClient(cf clientFactory, fn func(context.Context, orchdv1.OrchdClient) error) error {
	client, cleanup, err := cf()
	if err != nil {
		return err
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return fn(ctx, client)
}

func printProblems(out io.Writer, ps []*orchdv1.SpacesProblem) {
	if len(ps) == 0 {
		return
	}
	fmt.Fprintf(out, "%d problem(s); it cannot be applied until they are fixed:\n", len(ps))
	for _, p := range ps {
		fmt.Fprintf(out, "  [%s] %s\n", p.GetRule(), p.GetMessage())
	}
}
