package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func newSpacesScanCmd(cf clientFactory) *cobra.Command {
	var offline bool
	var depth int32
	cmd := &cobra.Command{
		Use:   "scan <folder>...",
		Short: "Look through folders for checkouts and GitHub orgs to place in spaces (operator only)",
		Long: `What the Spaces pane's setup scans: every git checkout under the folders, with
its GitHub owner, visibility and where the applied policy places it, and the
orgs each gh login belongs to. A scan reads and changes nothing; write the
policy you want and save it with 'orch spaces draft'.

Folders are walked without following symbolic links, skipping hidden
folders, at most --depth levels down (default 3, at most 6). --offline asks
GitHub nothing. orchd answers only the operator (refused inside an agent
session and inside tasks orchd runs; under a policy, only Orch.app or an
unlocked terminal).`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			folders := make([]string, len(args))
			for i, a := range args {
				f, err := whyTarget(a)
				if err != nil {
					return err
				}
				// Reason: orchd does not share this process's directory.
				if f, err = filepath.Abs(f); err != nil {
					return err
				}
				folders[i] = f
			}
			if depth < 0 || depth > 6 {
				return errors.New("--depth is 1 to 6 (0 means 3)")
			}
			return withClient(cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
				r, err := client.ScanSpaces(ctx, &orchdv1.ScanSpacesRequest{Folders: folders, MaxDepth: depth, Offline: offline})
				if err != nil {
					return err
				}
				printSpacesScan(c.OutOrStdout(), r)
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&offline, "offline", false, "ask GitHub nothing: visibility only from orchd's cache, no org lists")
	cmd.Flags().Int32Var(&depth, "depth", 0, "folders below each one to look through (0 means 3, at most 6)")
	return cmd
}

func printSpacesScan(out io.Writer, r *orchdv1.ScanSpacesResponse) {
	fmt.Fprintf(out, "%d checkout(s)\n", len(r.GetCheckouts()))
	for _, ch := range r.GetCheckouts() {
		remote := "no GitHub remote"
		if ch.GetOwner() != "" {
			remote = ch.GetOwner() + "/" + ch.GetRepo()
			if v := ch.GetVisibility(); v != "" {
				remote += " (" + v + ")"
			}
		}
		if ch.GetAnonymous() {
			remote = "remote not believed: placed by its folder"
		}
		fmt.Fprintf(out, "  %s  %s\n", shown(ch.GetPath()), shown(remote))
		if o := ch.GetOutcome(); o != "" {
			line := o
			if s := ch.GetSpace(); s != "" {
				line += " in " + s
			}
			if rule := ch.GetRule(); rule != "" {
				line += " by " + rule
			}
			if why := ch.GetReason(); why != "" {
				line += ": " + why
			}
			fmt.Fprintf(out, "    %s\n", shown(line))
		}
	}
	for _, a := range r.GetAccounts() {
		profile := "default gh login"
		if p := a.GetProfile(); p != "" {
			profile = "gh login of space " + p
		}
		line := profile + ": "
		switch {
		case a.GetError() != "":
			line += "could not be read: " + a.GetError()
		case a.GetLogin() == "":
			line += "not logged in"
		default:
			line += a.GetLogin()
			if orgs := a.GetOrgs(); len(orgs) > 0 {
				line += ", orgs " + strings.Join(orgs, ", ")
			}
		}
		fmt.Fprintln(out, shown(line))
	}
	for _, p := range r.GetProblems() {
		fmt.Fprintf(out, "could not read: %s\n", shown(p))
	}
	if r.GetTruncated() {
		fmt.Fprintln(out, "the scan stopped at a limit: name fewer or deeper folders")
	}
}
