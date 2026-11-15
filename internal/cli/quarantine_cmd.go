package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// quarantineTimeout bounds one quarantine command: list and purge scan the
// quarantine collection.
const quarantineTimeout = 2 * time.Minute

// RetagRequest is a retag of one source into a space, in the shape of
// orchd's RetagSource RPC (SP-071). ExpectFrom makes orchd refuse the retag
// unless the source is still in that space, so an assign never moves a
// source that left quarantine since it was listed.
type RetagRequest struct {
	SourceURI  string
	Space      string
	Confirm    bool
	ExpectFrom string
}

// RetagResult is what a retag moved.
type RetagResult struct {
	Chunks int
}

// Retagger moves a source to a space and records the sticky override that
// later refreshes honor. orchd's RetagSource (SP-071) implements it; assign
// does not move data itself.
type Retagger interface {
	Retag(ctx context.Context, req RetagRequest) (RetagResult, error)
}

// errRetagUnavailable is the assign answer until orchd has RetagSource.
var errRetagUnavailable = errors.New("assign needs orchd's RetagSource (SP-071), which this build does not have yet")

// unavailableRetagger is the Retagger before SP-071: it refuses every retag.
// Reason: SP-071 builds the move and its override rule; assign is only the
// quarantine front end to it, so it waits rather than duplicating the move.
type unavailableRetagger struct{}

func (unavailableRetagger) Retag(context.Context, RetagRequest) (RetagResult, error) {
	return RetagResult{}, errRetagUnavailable
}

// retaggerFactory makes the Retagger for a connected client.
type retaggerFactory func(orchdv1.OrchdClient) Retagger

// newQuarantineCmd is 'orch quarantine': list, assign and purge what orchd
// could not place (SP-028). orchd refuses all three to anyone but the
// operator.
func newQuarantineCmd(cf clientFactory, rf retaggerFactory) *cobra.Command {
	if rf == nil {
		rf = func(orchdv1.OrchdClient) Retagger { return unavailableRetagger{} }
	}
	cmd := &cobra.Command{
		Use:   "quarantine",
		Short: "List, assign or purge content that could not be placed in a space",
		Long: `Quarantine holds content orchd could not place in a space; no session reads it.
Only the operator can list, assign or purge it: from Orch.app, or where the policy allows
a terminal.`,
	}
	cmd.AddCommand(newQuarantineListCmd(cf), newQuarantineAssignCmd(cf, rf), newQuarantinePurgeCmd(cf))
	return cmd
}

func newQuarantineListCmd(cf clientFactory) *cobra.Command {
	return operatorOnlyMsg("queue tasks cannot list quarantine", &cobra.Command{
		Use:   "list",
		Short: "List the quarantined sources and their chunk counts",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return withQuarantineClient(c, cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
				resp, err := client.ListQuarantine(ctx, &orchdv1.ListQuarantineRequest{})
				if err != nil {
					return err
				}
				writeQuarantineList(c.OutOrStdout(), resp)
				return nil
			})
		},
	})
}

// writeQuarantineList prints resp as a table, then its totals.
func writeQuarantineList(w io.Writer, resp *orchdv1.ListQuarantineResponse) {
	if resp.GetTotalSources() == 0 {
		fmt.Fprintln(w, "quarantine is empty")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "CHUNKS\tTYPE\tINGESTED\tSOURCE")
	for _, s := range resp.GetSources() {
		at := "-"
		if s.GetIngestedAt() > 0 {
			at = time.Unix(s.GetIngestedAt(), 0).Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", s.GetChunks(), termSafe(s.GetSourceType()), at, termSafe(s.GetSourceUri()))
	}
	_ = tw.Flush()
	fmt.Fprintf(w, "%d source(s), %d chunk(s) in quarantine", resp.GetTotalSources(), resp.GetTotalChunks())
	if n := int32(len(resp.GetSources())); n < resp.GetTotalSources() {
		fmt.Fprintf(w, "; showing the first %d", n)
	}
	fmt.Fprintln(w)
}

func newQuarantineAssignCmd(cf clientFactory, rf retaggerFactory) *cobra.Command {
	var confirm bool
	cmd := operatorOnlyMsg("queue tasks cannot assign quarantined content", &cobra.Command{
		Use:   "assign <source_uri> <space>",
		Short: "Move a quarantined source to a space, and keep it there on later refreshes",
		Long: `Moves every chunk of a quarantined source to the space's collection without
re-embedding it, and records an override so later refreshes and re-ingests of the same
source land in that space. This is a retag (SP-071) of a source that is in quarantine;
assigning to the public space needs --confirm.`,
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			uri, space := args[0], args[1]
			if space == spaces.Quarantine {
				return errors.New("the source is already in quarantine; name the space to move it to")
			}
			return withQuarantineClient(c, cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
				// Reason: ListQuarantine is operator-only, so a caller that may
				// not see quarantine is refused before any retag is asked for.
				held, err := client.ListQuarantine(ctx, &orchdv1.ListQuarantineRequest{})
				if err != nil {
					return err
				}
				if !quarantineHolds(held, uri) {
					return fmt.Errorf("%s: not in quarantine", termSafe(uri))
				}
				res, err := rf(client).Retag(ctx, RetagRequest{SourceURI: uri, Space: space, Confirm: confirm, ExpectFrom: spaces.Quarantine})
				if err != nil {
					return err
				}
				fmt.Fprintf(c.OutOrStdout(), "moved %d chunk(s) of %s to %s; later refreshes keep it there\n",
					res.Chunks, termSafe(uri), termSafe(space))
				return nil
			})
		},
	})
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm a move that needs it, such as into the public space")
	return cmd
}

// quarantineHolds reports whether resp lists uri. A list cut short at
// orchd's limit may not name it, so the source is then looked for no
// further: assign refuses rather than retagging a source it could not see.
func quarantineHolds(resp *orchdv1.ListQuarantineResponse, uri string) bool {
	for _, s := range resp.GetSources() {
		if s.GetSourceUri() == uri {
			return true
		}
	}
	return false
}

func newQuarantinePurgeCmd(cf clientFactory) *cobra.Command {
	var all, confirm bool
	cmd := operatorOnlyMsg("queue tasks cannot purge quarantine", &cobra.Command{
		Use:   "purge [source_uri...]",
		Short: "Delete quarantined sources (dry run unless --confirm)",
		Long: `Deletes the named quarantined sources, or every one with --all. Without --confirm it
only counts what would be deleted. A named source that is not in quarantine refuses the
whole purge; nothing outside quarantine is ever deleted. Deletion is permanent.`,
		RunE: func(c *cobra.Command, args []string) error {
			if all == (len(args) > 0) {
				return errors.New("name the sources to purge, or use --all")
			}
			return withQuarantineClient(c, cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
				resp, err := client.PurgeQuarantine(ctx, &orchdv1.PurgeQuarantineRequest{SourceUris: args, All: all, Confirm: confirm})
				if err != nil {
					return err
				}
				out := c.OutOrStdout()
				if !resp.GetPurged() {
					fmt.Fprintf(out, "would purge %d source(s), %d chunk(s); run again with --confirm to delete them\n", resp.GetSources(), resp.GetChunks())
					return nil
				}
				fmt.Fprintf(out, "purged %d source(s), %d chunk(s)\n", resp.GetSources(), resp.GetChunks())
				return nil
			})
		},
	})
	cmd.Flags().BoolVar(&all, "all", false, "purge every quarantined source")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "delete; without it the purge only counts")
	return cmd
}

// withQuarantineClient runs fn with a connected client and a bounded
// context, and makes orchd's error text terminal-safe.
func withQuarantineClient(c *cobra.Command, cf clientFactory, fn func(context.Context, orchdv1.OrchdClient) error) error {
	client, cleanup, err := cf()
	if err != nil {
		return err
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(c.Context(), quarantineTimeout)
	defer cancel()
	if err := fn(ctx, client); err != nil {
		return errors.New(termSafe(err.Error()))
	}
	return nil
}
