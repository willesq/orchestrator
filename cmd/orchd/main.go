// Command orchd is the always-on orchestrator daemon entrypoint. It wires signal
// handling and delegates to internal/daemon, which holds the testable logic.
package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"github.com/willesq/orchestrator/internal/daemon"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := daemon.Run(ctx); err != nil {
		log.Fatalf("orchd: %v", err)
	}
}
