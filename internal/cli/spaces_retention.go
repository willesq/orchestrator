package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// retentionKey is the command log's retention setting, which the Spaces
// pane's setup sets (SP-052) and 'orch spaces log retention' shows or sets.
const retentionKey = "sessions.command_log.retention"

func newSpacesLogRetentionCmd(cf clientFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "retention [3d|7d|30d]",
		Short: "Show or set how long ended sessions' command logs are kept (setting is operator only)",
		Long: `With no argument, shows how long a session's command log is kept after it
ends. With a value, sets it, as the Spaces pane's setup does. Refused calls
are kept 90 days whatever this says.

Setting it is refused inside an agent session (Claude Code, Codex) and inside
tasks orchd runs; while a policy is applied, only Orch.app may change it.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			out := c.OutOrStdout()
			return withClient(cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
				if len(args) == 1 {
					raw, err := json.Marshal(args[0])
					if err != nil {
						return err
					}
					// The app confirms it by applying setup; here, by naming the value.
					if _, err := client.SetConfig(ctx, &orchdv1.SetConfigRequest{
						Values: map[string]string{retentionKey: string(raw)}, ConfirmSensitive: true,
					}); err != nil {
						return err
					}
					fmt.Fprintf(out, "ended sessions' command logs are now kept %s\n", args[0])
					return nil
				}
				st, err := client.GetConfig(ctx, &orchdv1.GetConfigRequest{})
				if err != nil {
					return err
				}
				for _, f := range st.GetFields() {
					if f.GetKey() != retentionKey {
						continue
					}
					var v string
					if json.Unmarshal([]byte(f.GetValue()), &v) != nil {
						v = f.GetValue()
					}
					fmt.Fprintf(out, "ended sessions' command logs are kept %s (choices: %s)\n", shown(v), shown(strings.Join(f.GetOptions(), ", ")))
					return nil
				}
				return fmt.Errorf("orchd does not report %s", retentionKey)
			})
		},
	}
}
