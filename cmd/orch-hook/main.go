// Command orch-hook is the single binary all harness lifecycle hooks invoke,
// registered as `orch-hook <event>`. Logic lives in internal/hook.
package main

import (
	"os"

	"github.com/willesq/orchestrator/internal/hook"
)

func main() {
	_ = hook.Run(os.Args[1:], os.Stdin, os.Stdout)
}
