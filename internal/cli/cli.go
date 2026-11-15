// Package cli implements the `orch` command tree. Every command is a thin client
// that dials orchd over its Unix-domain socket. The orchd client is injected via
// a clientFactory so the command tree is testable without a running daemon.
package cli

import (
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/willesq/orchestrator/internal/paths"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// clientFactory dials orchd and returns a client plus a cleanup func.
type clientFactory func() (orchdv1.OrchdClient, func(), error)

// Execute runs the root command against the real orchd socket.
func Execute() error {
	return NewRootCmd(dialOrchd).Execute()
}

// NewRootCmd builds the command tree using the given client factory.
func NewRootCmd(cf clientFactory) *cobra.Command {
	root := &cobra.Command{
		Use:           "orch",
		Short:         "Claude Orchestrator — local-first agent orchestration",
		SilenceUsage:  true,
		SilenceErrors: true,
		// Reason: an app launched from Finder, and launchd, run orch with
		// PATH=/usr/bin:/bin:/usr/sbin:/sbin, which hides gh and brew; orchd
		// widens its PATH the same way at startup.
		PersistentPreRunE: func(*cobra.Command, []string) error { return paths.EnsureToolPATH() },
	}
	root.AddCommand(
		newVersionCmd(),
		newConfigCmd(),
		newPingCmd(cf),
		newStatusCmd(cf),
		newLogsCmd(cf),
		newRestartCmd(cf),
		newDaemonCmd(cf),
		newIngestCmd(cf),
		newSearchCmd(cf),
		newRAGCmd(cf),
		newDispatchCmd(cf),
		newTasksCmd(cf),
		newTaskCmd(cf),
		newMCPCmd(cf),
		newInstallCmd(),
		newCheckCmd(),
		newModelCmd(),
		newProviderCmd(),
		newRoutingCmd(),
		newQdrantCmd(),
		newSourceCmd(cf),
		newDocsCmd(cf),
		newPRsCmd(cf),
		newGitHubCmd(cf),
		newRepoCmd(),
		newSettingsCmd(cf),
		newSpacesCmd(cf),
		newQuarantineCmd(cf, nil),
		newGuardCmd(),
		newSessionsCmd(cf),
		newRunCmd(cf),
	)
	return root
}

func dialOrchd() (orchdv1.OrchdClient, func(), error) {
	conn, err := grpc.NewClient(
		"unix://"+paths.SocketPath(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, nil, err
	}
	return orchdv1.NewOrchdClient(conn), func() { _ = conn.Close() }, nil
}
