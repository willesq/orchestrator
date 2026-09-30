package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// Pseudo-components `orch restart` accepts alongside the live subsystem names.
const (
	componentOrchd      = "orchd"
	componentSubsystems = "subsystems"
)

// newRestartCmd builds `orch restart [component]`.
//
// With no component it bounces the orchd process through its service manager,
// which restarts every subsystem and the orchd-managed qdrant engine with it.
// Naming a subsystem restarts just that one in place over the RPC, leaving the
// socket, the warm embedder and the other subsystems untouched.
func newRestartCmd(cf clientFactory) *cobra.Command {
	var list bool
	cmd := &cobra.Command{
		Use:   "restart [component]",
		Short: "Restart every component, or just one subsystem",
		Long: `Restart the orchestrator.

With no component, the orchd process is restarted via its service manager, which
takes every supervised subsystem (and the orchd-managed qdrant engine) down and
back up, and picks up config and binary changes.

orchd already watches its own binary and restarts itself within a minute of a new
build being installed, so this is the manual way to close the gap when that did
not happen — which 'orch status' reports as a drifted build.

Components:
  orchd        the daemon process — the default, same as passing nothing
  subsystems   every supervised subsystem, in place, without bouncing the process
  <name>       one subsystem, in place (run 'orch restart --list' for the names)

Restarting is idempotent: a component that is already stopped is simply started.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			w := c.OutOrStdout()
			if list {
				return listComponents(c.Context(), cf, w)
			}
			target := componentOrchd
			if len(args) == 1 {
				target = args[0]
			}
			switch target {
			case componentOrchd:
				return restartProcess(cf, w)
			case componentSubsystems:
				return restartSubsystem(c.Context(), cf, w, "")
			default:
				return restartSubsystem(c.Context(), cf, w, target)
			}
		},
	}
	cmd.Flags().BoolVar(&list, "list", false, "list the components that can be restarted")
	return cmd
}

// restartProcess bounces orchd through its service manager, then waits for it to
// answer again so the command only reports success once the system is back.
func restartProcess(cf clientFactory, w io.Writer) error {
	if err := superviseDaemon(w, "restart"); err != nil {
		return err
	}
	resp, err := waitForOrchd(cf, 30*time.Second)
	if err != nil {
		return fmt.Errorf("orchd was restarted but did not come back within 30s: %w", err)
	}
	fmt.Fprintf(w, "orchd %s is back up; %d subsystem(s) restarted with it\n",
		resp.GetVersion(), len(resp.GetSubsystems()))
	return nil
}

// restartSubsystem restarts one subsystem (or all of them, for an empty name) in
// the running daemon.
func restartSubsystem(ctx context.Context, cf clientFactory, w io.Writer, name string) error {
	client, cleanup, err := cf()
	if err != nil {
		return err
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	resp, err := client.Restart(ctx, &orchdv1.RestartRequest{Name: name})
	if err != nil {
		return restartError(err)
	}
	restarted := resp.GetRestarted()
	if len(restarted) == 1 {
		fmt.Fprintf(w, "restarted subsystem %q in place (no process bounce)\n", restarted[0])
		return nil
	}
	fmt.Fprintf(w, "restarted %d subsystem(s) in place (no process bounce): %s\n",
		len(restarted), strings.Join(restarted, ", "))
	return nil
}

// listComponents prints what `orch restart` accepts, taking the subsystem names
// from the running daemon so the list can never drift from reality.
func listComponents(ctx context.Context, cf clientFactory, w io.Writer) error {
	type row struct{ name, detail string }
	rows := []row{
		{componentOrchd, "the daemon process (default) — restarts everything, onto the installed build"},
		{componentSubsystems, "every subsystem, in place"},
	}

	client, cleanup, err := cf()
	if err != nil {
		return err
	}
	defer cleanup()
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	resp, statusErr := client.Status(rctx, &orchdv1.StatusRequest{})
	if statusErr == nil {
		for _, s := range resp.GetSubsystems() {
			rows = append(rows, row{s.GetName(), fmt.Sprintf("%s (%s)", s.GetDetail(), s.GetState())})
		}
	}

	width := 0
	for _, r := range rows {
		if n := len(r.name); n > width {
			width = n
		}
	}
	for _, r := range rows {
		fmt.Fprintf(w, "%-*s  %s\n", width, r.name, r.detail)
	}
	if statusErr != nil {
		fmt.Fprintln(w, "\n(orchd is not running — start it to list its subsystems)")
	}
	return nil
}

// restartError turns a failed Restart RPC into an actionable message: an
// unreachable daemon is the common case and has a different fix than a wedged
// subsystem.
func restartError(err error) error {
	st, ok := grpcstatus.FromError(err)
	if !ok {
		return err
	}
	switch st.Code() {
	case codes.Unavailable:
		// The raw message is a gRPC dial error; the fix is what matters here.
		return fmt.Errorf("orchd is not reachable — run 'orch restart' with no component to restart the whole system")
	case codes.NotFound:
		return fmt.Errorf("%s", st.Message())
	default:
		return fmt.Errorf("restart failed: %s", st.Message())
	}
}

// waitForOrchd polls Status until the daemon answers or the deadline passes.
func waitForOrchd(cf clientFactory, timeout time.Duration) (*orchdv1.StatusResponse, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := doStatus(cf, 2*time.Second)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		time.Sleep(250 * time.Millisecond)
	}
	return nil, lastErr
}

// doStatus is a one-shot Status call with its own timeout.
func doStatus(cf clientFactory, timeout time.Duration) (*orchdv1.StatusResponse, error) {
	client, cleanup, err := cf()
	if err != nil {
		return nil, err
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return client.Status(ctx, &orchdv1.StatusRequest{})
}
