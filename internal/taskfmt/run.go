package taskfmt

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// Run describes whether anything is working on a pending or in-progress task,
// or "" for a task that has ended. now anchors the relative times.
func Run(t *orchdv1.Task, now time.Time) string {
	r := t.GetRun()
	if r == nil {
		return ""
	}
	var b strings.Builder
	switch r.GetState() {
	case "running":
		fmt.Fprintf(&b, "run:     running (pid %d", r.GetPid())
		if r.GetStartedAt() > 0 {
			fmt.Fprintf(&b, ", for %s", since(now, r.GetStartedAt()))
		}
		b.WriteString(")\n")
	case "waiting":
		b.WriteString("run:     waiting for a worker to pick it up (queued, or resuming after a restart)\n")
	default:
		fmt.Fprintf(&b, "run:     ⚠ stalled — nothing is running it; `orch task %s --retry` or `--cancel`\n", t.GetId())
	}
	if r.GetLastActivity() > 0 {
		fmt.Fprintf(&b, "active:  %s ago (session transcript last changed)\n", since(now, r.GetLastActivity()))
	}
	if r.GetSessionId() != "" {
		fmt.Fprintf(&b, "session: %s\n", printable(r.GetSessionId()))
	}
	if r.GetCwd() != "" {
		fmt.Fprintf(&b, "cwd:     %s\n", printable(r.GetCwd()))
	}
	if r.GetResumeCommand() != "" {
		fmt.Fprintf(&b, "open it: %s\n", printable(r.GetResumeCommand()))
	}
	return b.String()
}

// printable drops control characters, so a directory name cannot carry
// terminal escape sequences into the user's terminal.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// RunTag is a short marker for list output: "" while running or waiting,
// "stalled" when nothing owns an unsettled task.
func RunTag(t *orchdv1.Task) string {
	if r := t.GetRun(); r != nil && r.GetState() == "stalled" {
		return "⚠ stalled"
	}
	return ""
}

func since(now time.Time, unix int64) string {
	d := now.Sub(time.Unix(unix, 0)).Round(time.Second)
	if d < 0 {
		d = 0
	}
	return d.String()
}
