package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func newSpacesWhyCmd(cf clientFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "why <path|url|owner/repo|slack:<id>|teams:<id>>",
		Short: "Show which space a target's content goes to, and why",
		Long: `Shows every bound root holding the target from shallowest to deepest,
what its GitHub remote contributes, the rule that decided, and the result.
A path starts with /, ~ or . (write ./dir for a folder here); anything else
without a scheme is read as owner/repo. Inside an agent session orchd answers
only when every space in the answer is public.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			target, err := whyTarget(args[0])
			if err != nil {
				return err
			}
			return withClient(cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
				e, err := client.ExplainPlacement(ctx, &orchdv1.ExplainPlacementRequest{Target: target})
				if err != nil {
					return err
				}
				printPlacement(c.OutOrStdout(), target, e)
				return nil
			})
		},
	}
}

// whyTarget makes a path target absolute, since orchd does not share the
// caller's working directory.
func whyTarget(t string) (string, error) {
	switch {
	case t == "~" || strings.HasPrefix(t, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, strings.TrimPrefix(t, "~")), nil
	case t == "." || t == ".." || strings.HasPrefix(t, "./") || strings.HasPrefix(t, "../"):
		return filepath.Abs(t)
	}
	return t, nil
}

func printPlacement(out io.Writer, target string, e *orchdv1.PlacementExplanation) {
	fmt.Fprintf(out, "target: %s (%s)\n", target, e.GetTargetKind())
	if roots := e.GetRoots(); len(roots) > 0 {
		fmt.Fprintln(out, "roots, shallowest first:")
		for i, r := range roots {
			mark := ""
			if i == len(roots)-1 {
				mark = "  <- deepest, counts"
			}
			fmt.Fprintf(out, "  %s  %s (%s)%s\n", r.GetRoot(), r.GetSpace(), r.GetKind(), mark)
		}
	} else if e.GetTargetKind() == "path" {
		fmt.Fprintln(out, "roots: none hold it")
	}
	if r := e.GetRemote(); r != nil {
		vis := r.GetVisibility()
		if vis == "" {
			vis = "visibility not checked"
		}
		fmt.Fprintf(out, "remote: %s/%s (%s): %s\n", r.GetOwner(), r.GetRepo(), vis, r.GetEffect())
	} else if e.GetTargetKind() == "path" {
		fmt.Fprintln(out, "remote: none")
	}
	fmt.Fprintf(out, "rule: %s: %s\n", e.GetRule(), e.GetReason())
	fmt.Fprintf(out, "result: %s (%s)\n", e.GetSpace(), e.GetOutcome())
	if s := e.GetSessionSpace(); s != "" {
		reads := e.GetSessionReads()
		if reads == "" {
			reads = "no space (public only)"
		}
		fmt.Fprintf(out, "a session started here: content goes to %s; reads as %s\n", s, reads)
	}
}
