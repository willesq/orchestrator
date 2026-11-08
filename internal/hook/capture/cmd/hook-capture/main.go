// Command hook-capture is the throwaway logging hook used by
// scripts/hook-capture.sh: hook-capture <dir> <harness> <event>.
package main

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/willesq/orchestrator/internal/hook/capture"
)

func main() {
	// Reason: a capture hook must never block or fail the observed session, so
	// every path exits zero and problems go to stderr only.
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: hook-capture <dir> <harness> <event>")
		return
	}
	stdin, _ := io.ReadAll(os.Stdin)
	cwd, _ := os.Getwd()
	out, err := capture.Capture(capture.Options{
		Dir: os.Args[1], Harness: os.Args[2], Event: os.Args[3], HookCwd: cwd, Now: time.Now(),
	}, stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hook-capture:", err)
	}
	if out != "" {
		fmt.Println(out)
	}
}
