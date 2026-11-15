package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// newSpacesTermsCmd groups the outbound terms commands (spaces SP-075).
func newSpacesTermsCmd(cf clientFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "terms",
		Short: "Outbound terms check commands",
	}
	cmd.AddCommand(newSpacesTermsRulesCmd(cf))
	return cmd
}

// newSpacesTermsRulesCmd lists and changes the outbound check's rules.
func newSpacesTermsRulesCmd(cf clientFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rules",
		Short: "List or change the outbound terms check's rules (changing is operator only)",
		Long: `The outbound terms check refuses a tool call that would send one space's
terms to a destination outside it. How strictly it reads commands is a
baseline of named rules, all on by default. You may switch any baseline
rule off (relaxing the check) or back on, and add stricter rules of your own:

  command <program>             check the program's words as text sent to
                                a destination that holds no space (glab)
  destination <owner[/repo]>    no space's terms may reach this GitHub owner
                                or repo, its own org's included
  fail_closed <text|re:regex>   refuse any tool call whose tool name or text
                                matches (an MCP tool, curl to the API)

An added rule can only make the check stricter; one that would allow or
exempt anything is rejected. To turn the whole check off, switch the terms
layer: 'orch spaces enforce terms off [--for 15m]'.

A rule's value is never shown again, logged or put in a refusal: rules are
named by id (user-1, user-2, ...). Keep your own note of what each holds.
'add' reads the value from its input (one line), so it stays out of your
shell history and the process list: pipe it from a file, or type it at the
prompt.

  orch spaces terms rules [list]
  orch spaces terms rules enable <id>
  orch spaces terms rules disable <id>
  orch spaces terms rules add <kind> < value-file
  orch spaces terms rules remove <id>

Changing a rule is refused inside an agent session (Claude Code, Codex) and
inside tasks orchd runs: run it in your own terminal or use Orch.app.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error { return listOutboundRules(c, cf) },
	}
	list := &cobra.Command{
		Use: "list", Short: "List the rules and their state", Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error { return listOutboundRules(c, cf) },
	}
	change := func(use, short, action string, nargs int) *cobra.Command {
		return &cobra.Command{
			Use: use, Short: short, Args: cobra.ExactArgs(nargs),
			RunE: func(c *cobra.Command, args []string) error {
				req := &orchdv1.ChangeOutboundRuleRequest{Action: action}
				if action == "add" {
					v, err := readRuleValue(c)
					if err != nil {
						return err
					}
					req.Kind, req.Value = args[0], v
				} else {
					req.Id = args[0]
				}
				return changeOutboundRule(c, cf, req)
			},
		}
	}
	cmd.AddCommand(list,
		change("enable <id>", "Switch a rule on (operator only)", "enable", 1),
		change("disable <id>", "Switch a rule off (operator only)", "disable", 1),
		change("add <kind>", "Add a stricter rule: command, destination or fail_closed, its value read from input (operator only)", "add", 1),
		change("remove <id>", "Remove a rule you added (operator only)", "remove", 1))
	return cmd
}

// readRuleValue reads an added rule's value: the first line of the
// command's input. Reason: an argument would put it in shell history, the
// process list and, typed in an agent session, the command log (INV-07).
func readRuleValue(c *cobra.Command) (string, error) {
	fmt.Fprint(c.ErrOrStderr(), "rule value (one line, never shown again): ")
	line, err := bufio.NewReader(io.LimitReader(c.InOrStdin(), 4096)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	fmt.Fprintln(c.ErrOrStderr())
	v := strings.TrimSpace(line)
	if v == "" {
		return "", errors.New("no rule value on input: pipe it in, e.g. orch spaces terms rules add fail_closed < value.txt")
	}
	return v, nil
}

func listOutboundRules(c *cobra.Command, cf clientFactory) error {
	return withClient(cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
		rules, err := client.ListOutboundRules(ctx, &orchdv1.ListOutboundRulesRequest{})
		if err != nil {
			return err
		}
		printOutboundRules(c.OutOrStdout(), rules)
		return nil
	})
}

func changeOutboundRule(c *cobra.Command, cf clientFactory, req *orchdv1.ChangeOutboundRuleRequest) error {
	out := c.OutOrStdout()
	return withClient(cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
		rep, err := client.ChangeOutboundRule(ctx, req)
		if err != nil {
			return err
		}
		verb := map[string]string{"enable": "switched on", "disable": "switched off", "add": "added", "remove": "removed"}[req.GetAction()]
		if rep.GetChanged() {
			fmt.Fprintf(out, "outbound rule %s %s (rules revision %d)\n", rep.GetRuleId(), verb, rep.GetRevision())
		} else {
			fmt.Fprintf(out, "outbound rule %s was already %s\n", rep.GetRuleId(), verb)
		}
		return nil
	})
}

// printOutboundRules prints the rules as a table, never a value.
func printOutboundRules(out io.Writer, r *orchdv1.OutboundRules) {
	if p := r.GetProblem(); p != "" {
		fmt.Fprintf(out, "warning: %s\n", p)
	}
	if st := r.GetTermsLayer(); st != "" && st != "on" {
		fmt.Fprintf(out, "the terms layer is %s: no rule applies ('orch spaces enforce terms on')\n", st)
	}
	fmt.Fprintf(out, "%-28s %-12s %-4s %s\n", "RULE", "KIND", "ON", "WHAT IT DOES")
	for _, rule := range r.GetRules() {
		on := "on"
		if !rule.GetOn() {
			on = "off"
		}
		what := rule.GetSummary()
		if rule.GetLoosens() {
			what += " (loosens the strict reading)"
		}
		fmt.Fprintf(out, "%-28s %-12s %-4s %s\n", rule.GetId(), rule.GetKind(), on, what)
	}
	if rev := r.GetRevision(); rev > 0 {
		line := fmt.Sprintf("rules revision %d, changed %s", rev, time.Unix(r.GetSetAt(), 0).Local().Format("2006-01-02 15:04"))
		if by := r.GetSetBy(); by != "" {
			line += " by " + by
		}
		fmt.Fprintln(out, line)
	} else {
		fmt.Fprintln(out, "the baseline: no rule was ever changed")
	}
	if h := r.GetHistory(); len(h) > 0 {
		fmt.Fprintln(out, "recent changes:")
		for _, ch := range h {
			fmt.Fprintf(out, "  %s  %-7s %-28s by %s\n", time.Unix(ch.GetAt(), 0).Local().Format("2006-01-02 15:04"),
				ch.GetAction(), ch.GetRuleId(), ch.GetBy())
		}
	}
	if k := r.GetKinds(); len(k) > 0 {
		fmt.Fprintf(out, "add a stricter rule: orch spaces terms rules add <%s> (value on input)\n", strings.Join(k, "|"))
	}
}
