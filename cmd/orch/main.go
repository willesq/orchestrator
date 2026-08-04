// Command orch is the orchestrator CLI entrypoint. All logic lives in
// internal/cli so it is testable without a running daemon.
package main

import (
	"fmt"
	"os"

	"github.com/willesq/orchestrator/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
