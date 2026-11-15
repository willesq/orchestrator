package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// The terms files' commands (SP-055): the CLI side of the Spaces editor's
// write-only terms field (SP-053). A term is never printed: orchd answers
// with counts and fingerprints only (INV-07).

// maxTermsInput is one byte over orchd's terms file limit, so an oversized
// file is refused by orchd with its message rather than cut short here.
const maxTermsInput = 256<<10 + 1

func newSpacesTermsListCmd(cf clientFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List each space's terms file with its term count and fingerprint, never a term (operator only)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return withClient(cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
				r, err := client.GetSpaceTerms(ctx, &orchdv1.GetSpaceTermsRequest{})
				if err != nil {
					return err
				}
				printSpaceTerms(c.OutOrStdout(), r.GetSpaces())
				return nil
			})
		},
	}
}

func printSpaceTerms(out io.Writer, list []*orchdv1.SpaceTerms) {
	if len(list) == 0 {
		fmt.Fprintln(out, "no applied space: terms files belong to an applied policy's spaces")
		return
	}
	for _, t := range list {
		var state string
		switch {
		case t.GetTermsFile() == "":
			state = "no terms file named in the policy"
		case t.GetProblem() != "":
			state = t.GetProblem()
		case !t.GetExists():
			state = "file missing"
		default:
			state = fmt.Sprintf("%d term(s), fingerprint %s", t.GetCount(), shown(t.GetFingerprint()))
		}
		line := shown(t.GetSpace())
		if f := t.GetTermsFile(); f != "" {
			line += "  " + shown(f)
		}
		fmt.Fprintf(out, "%s\n  %s\n", line, shown(state))
	}
}

func newSpacesTermsSetCmd(cf clientFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "set <space> < terms-file",
		Short: "Replace a space's terms file with the terms on input (Orch.app only under a policy)",
		Long: `Replaces the applied space's terms file with the terms read from input, one
per line, as the Spaces editor's terms field does. The terms are never
printed: orchd answers with the new count and fingerprint.

Replacing terms can drop some, so while a policy is applied orchd accepts
this only from Orch.app, even in an unlocked terminal (D-78).`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			data, err := io.ReadAll(io.LimitReader(c.InOrStdin(), maxTermsInput))
			if err != nil {
				return err
			}
			if len(data) == 0 {
				return errors.New("no terms on input: pipe them in, e.g. orch spaces terms set <space> < terms.txt")
			}
			return withClient(cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
				t, err := client.SetSpaceTerms(ctx, &orchdv1.SetSpaceTermsRequest{Space: args[0], Terms: string(data)})
				if err != nil {
					return err
				}
				fmt.Fprintf(c.OutOrStdout(), "replaced the terms of %s: %d term(s), fingerprint %s\n",
					shown(t.GetSpace()), t.GetCount(), shown(t.GetFingerprint()))
				return nil
			})
		},
	}
}
