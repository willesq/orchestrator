package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/models"
	"github.com/willesq/orchestrator/internal/routing"
)

func newRoutingCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "routing",
		Short: "Rank models per task kind for dispatched work",
		Long: "The routing table ranks, per task kind, the enabled models a dispatch of that kind runs on.\n" +
			"evaluate proposes a table from the routing policy in config; apply writes it, keeping the\n" +
			"replaced table for rollback. set pins a kind by hand; apply leaves pinned kinds alone.",
	}
	cmd.AddCommand(newRoutingShowCmd(), newRoutingEvaluateCmd(), operatorOnly(newRoutingApplyCmd()),
		operatorOnly(newRoutingRollbackCmd()), operatorOnly(newRoutingSetCmd()), operatorOnly(newRoutingClearCmd()))
	return cmd
}

// operatorOnly refuses cmd inside a queued task. Routing and the registry decide
// which model every later task runs on, so a session that an untrusted
// repository may be steering must not change them; reading them is fine.
func operatorOnly(cmd *cobra.Command) *cobra.Command {
	cmd.PreRunE = func(*cobra.Command, []string) error {
		if id := os.Getenv(agentrun.TaskIDEnv); id != "" {
			return fmt.Errorf("queue tasks cannot change model routing (running as %s)", id)
		}
		return nil
	}
	return cmd
}

// openRouter opens orch.db and builds a router from the config's policy.
func openRouter() (*routing.Router, func(), error) {
	v, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	s, err := config.Unmarshal(v)
	if err != nil {
		return nil, nil, err
	}
	p := routing.PolicyFrom(s.Routing)
	if err := p.Validate(); err != nil {
		return nil, nil, err
	}
	conn, err := db.Open()
	if err != nil {
		return nil, nil, err
	}
	r := &routing.Router{Store: routing.NewStore(conn), Models: models.NewRegistry(conn), Policy: p}
	return r, func() { _ = conn.Close() }, nil
}

func newRoutingShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print the current routing table",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			r, closeFn, err := openRouter()
			if err != nil {
				return err
			}
			defer closeFn()
			table, err := r.Store.Table(c.Context())
			if err != nil {
				return err
			}
			if len(table) == 0 {
				fmt.Fprintln(c.OutOrStdout(), "routing table is empty; run `orch routing evaluate`, then `orch routing apply`")
				return nil
			}
			return printRoutes(c.OutOrStdout(), table)
		},
	}
}

func printRoutes(out io.Writer, routes []routing.Route) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "KIND\tRANK\tMODEL\tEVIDENCE\tWHY")
	for _, rt := range routes {
		why := rt.Rationale
		if rt.Pinned {
			why = "pinned: " + why
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%d\t%s\n", rt.Kind, rt.Rank, rt.Ref(), rt.EvidenceN, why)
	}
	return w.Flush()
}

// proposal ranks the enabled models under the policy.
func proposal(c *cobra.Command, r *routing.Router) ([]routing.Route, []routing.Route, error) {
	cands, err := r.Candidates(c.Context())
	if err != nil {
		return nil, nil, err
	}
	current, err := r.Store.Table(c.Context())
	if err != nil {
		return nil, nil, err
	}
	return current, routing.Propose(r.Policy, cands, time.Now().Unix()), nil
}

func printChanges(out io.Writer, changes []routing.Change) {
	for _, ch := range changes {
		switch {
		case ch.From == "":
			fmt.Fprintf(out, "+ %s #%d %s\n", ch.Kind, ch.Rank, ch.To)
		case ch.To == "":
			fmt.Fprintf(out, "- %s #%d %s\n", ch.Kind, ch.Rank, ch.From)
		default:
			fmt.Fprintf(out, "~ %s #%d %s → %s\n", ch.Kind, ch.Rank, ch.From, ch.To)
		}
	}
}

func newRoutingEvaluateCmd() *cobra.Command {
	var explain string
	cmd := &cobra.Command{
		Use:   "evaluate",
		Short: "Propose a routing table from the policy and show how it differs (writes nothing)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if explain != "" && !routing.ValidKind(explain) {
				return fmt.Errorf("unknown task kind %q", explain)
			}
			r, closeFn, err := openRouter()
			if err != nil {
				return err
			}
			defer closeFn()
			current, proposed, err := proposal(c, r)
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			if len(proposed) == 0 {
				fmt.Fprintln(out, "no enabled models to route to; see `orch model list`")
				return nil
			}
			if explain != "" {
				var mine []routing.Route
				for _, rt := range proposed {
					if rt.Kind == explain {
						mine = append(mine, rt)
					}
				}
				return printRoutes(out, mine)
			}
			changes := routing.Diff(current, proposed)
			if len(changes) == 0 {
				fmt.Fprintln(out, "no changes: the table matches the policy")
				return nil
			}
			printChanges(out, changes)
			fmt.Fprintf(out, "%d change(s) proposed; `orch routing apply` writes them (pinned kinds are kept)\n", len(changes))
			return nil
		},
	}
	cmd.Flags().StringVar(&explain, "explain", "", "show the full proposal and rationale for one task kind")
	return cmd
}

func newRoutingApplyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "apply",
		Short: "Write the proposed routing table, keeping the current one for rollback",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			r, closeFn, err := openRouter()
			if err != nil {
				return err
			}
			defer closeFn()
			current, proposed, err := proposal(c, r)
			if err != nil {
				return err
			}
			if len(proposed) == 0 {
				return errors.New("no enabled models to route to; see `orch model list`")
			}
			pinned, err := r.Store.Apply(c.Context(), proposed, time.Now().Unix())
			if err != nil {
				return err
			}
			after, err := r.Store.Table(c.Context())
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			printChanges(out, routing.Diff(current, after))
			fmt.Fprintf(out, "applied; %d pinned kind(s) kept; `orch routing rollback` restores the previous table\n", pinned)
			return nil
		},
	}
}

func newRoutingRollbackCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rollback",
		Short: "Restore the routing table as it was before the last change",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			r, closeFn, err := openRouter()
			if err != nil {
				return err
			}
			defer closeFn()
			if err := r.Store.Rollback(c.Context()); err != nil {
				return err
			}
			fmt.Fprintln(c.OutOrStdout(), "rolled back")
			return nil
		},
	}
}

func newRoutingSetCmd() *cobra.Command {
	var effort string
	cmd := &cobra.Command{
		Use:   "set <kind> <provider>/<slug>",
		Short: "Pin a task kind to one enabled model; apply leaves it alone until cleared",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			kind := args[0]
			if !routing.ValidKind(kind) {
				return fmt.Errorf("unknown task kind %q", kind)
			}
			r, closeFn, err := openRouter()
			if err != nil {
				return err
			}
			defer closeFn()
			rec, err := r.Pin(c.Context(), args[1], nil)
			if err != nil {
				return err
			}
			if effort != "" {
				m, err := r.Models.Get(c.Context(), rec.Provider, rec.Model)
				if err != nil {
					return err
				}
				if len(m.SupportedEfforts) == 0 || !m.SupportsEffort(effort) {
					return fmt.Errorf("%s does not support effort %q (supported: %s)", m.Ref(), effort, efforts(m))
				}
			}
			rt := routing.Route{Kind: kind, Provider: rec.Provider, Model: rec.Model, Effort: effort}
			if err := r.Store.Set(c.Context(), rt, time.Now().Unix()); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "%s pinned to %s\n", kind, rt.Ref())
			return nil
		},
	}
	cmd.Flags().StringVar(&effort, "effort", "", "reasoning effort, for models that declare levels")
	return cmd
}

func newRoutingClearCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clear <kind>",
		Short: "Remove a kind's routes (and any pin) so the next apply fills it",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			r, closeFn, err := openRouter()
			if err != nil {
				return err
			}
			defer closeFn()
			if err := r.Store.Clear(c.Context(), args[0], time.Now().Unix()); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "%s cleared\n", args[0])
			return nil
		},
	}
}
