// Command orchd is the always-on orchestrator daemon entrypoint. It wires signal
// handling and delegates to internal/daemon, which holds the testable logic.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/willesq/orchestrator/internal/daemon"
	"github.com/willesq/orchestrator/internal/feeds/slack"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// orchd runs itself as the Slack connector process, so a chat token is
	// read only in that child, never in the daemon (SP-061).
	if len(os.Args) == 2 && os.Args[1] == slack.Command {
		os.Exit(slack.Main(ctx, os.Stdin, os.Stdout))
	}
	if err := daemon.Run(ctx); err != nil {
		log.Fatalf("orchd: %v", err)
	}
}
