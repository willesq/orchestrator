package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/github"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// `orch spaces list` and `orch spaces scan`: the terminal side of the Spaces
// pane's Overview and setup scan (SP-055), so every pane action has a CLI
// equivalent. Both are reads; orchd answers them only to the operator.

func newSpacesListCmd(cf clientFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the applied spaces with their bindings, reads and logins (operator only)",
		Long: `Lists the applied policy's spaces as the Spaces pane's Overview shows them:
each space's kind, GitHub orgs, roots, web prefixes, chat tenants, the spaces
it may read, its terms file (never a term) and which gh login and Claude Code
configuration it uses, then where sessions pinned under a public root write.

orchd answers only the operator: refused inside an agent session (Claude
Code, Codex) and inside tasks orchd runs, and while a policy is applied only
to Orch.app or a terminal 'orch spaces unlock' unlocked.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return withClient(cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
				r, err := client.ListSpaces(ctx, &orchdv1.ListSpacesRequest{})
				if err != nil {
					return err
				}
				printSpacesList(c.OutOrStdout(), r)
				return nil
			})
		},
	}
}

// printSpacesList prints the policy's state, then each space.
func printSpacesList(out io.Writer, r *orchdv1.ListSpacesResponse) {
	st := r.GetStatus()
	if e := st.GetError(); e != "" {
		fmt.Fprintf(out, "warning: %s\n", shown(e))
	}
	if !st.GetConfigured() {
		fmt.Fprintln(out, "no spaces policy is applied: save one with 'orch spaces draft', or run setup in Orch.app")
		return
	}
	line := fmt.Sprintf("applied: revision %d", st.GetAppliedRevision())
	if d := st.GetDraftRevision(); d > 0 {
		line += fmt.Sprintf("; draft revision %d is not applied (orch spaces diff)", d)
	}
	fmt.Fprintln(out, line)
	if drift := st.GetDrift(); len(drift) > 0 {
		fmt.Fprintf(out, "drift: %s\n", shown(strings.Join(drift, ", ")))
	}
	logins := map[string]*orchdv1.SpaceLogin{}
	for _, l := range r.GetLogins() {
		logins[l.GetSpace()] = l
	}
	for _, sp := range r.GetSpaces() {
		fmt.Fprintf(out, "\n%s (%s)\n", shown(sp.GetName()), shown(sp.GetKind()))
		listField(out, "github orgs", sp.GetGithubOrgs())
		listField(out, "roots", sp.GetRoots())
		listField(out, "web prefixes", sp.GetWebPrefixes())
		listField(out, "chat", sp.GetChat())
		listField(out, "reads", sp.GetReadable())
		if f := sp.GetTermsFile(); f != "" {
			fmt.Fprintf(out, "  terms file:   %s\n", shown(f))
		}
		if l := logins[sp.GetName()]; l != nil {
			fmt.Fprintf(out, "  gh login:     %s\n", loginLine(l.GetGh()))
			fmt.Fprintf(out, "  claude:       %s\n", loginLine(l.GetClaude()))
		}
	}
	if p := r.GetPublicRootSessions(); p != "" {
		fmt.Fprintf(out, "\nsessions pinned under a public root write to %s\n", shown(p))
	}
}

func listField(out io.Writer, name string, values []string) {
	if len(values) == 0 {
		return
	}
	shownValues := make([]string, len(values))
	for i, v := range values {
		shownValues[i] = shown(v)
	}
	fmt.Fprintf(out, "  %-13s %s\n", name+":", strings.Join(shownValues, ", "))
}

// loginLine says which login a space uses and its state, never a token.
func loginLine(l *orchdv1.LoginState) string {
	if l == nil {
		return "unknown"
	}
	where := "the default"
	if l.GetOwn() {
		where = "its own (" + shown(l.GetDir()) + ")"
	}
	line := where + ": " + shown(orNone(l.GetState()))
	if a := l.GetAccount(); a != "" {
		line += " as " + shown(a)
	}
	if d := l.GetDetail(); d != "" {
		line += " (" + shown(d) + ")"
	}
	return line
}

// shown keeps a value orchd read from disk, a policy or GitHub from moving
// the terminal's cursor or colours.
func shown(s string) string { return github.Printable(s, 300) }
