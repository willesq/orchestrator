package hook

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/session"
)

// maxBriefFiles bounds the uncommitted files the brief lists.
const maxBriefFiles = 10

// writeResumeBrief reminds a session that was just compacted where it was. The
// harness's own summary keeps the conversation's gist; this adds what that
// summary cannot know or tends to blur: the repository's live state, and the
// last request and reply verbatim from the checkpoint PreCompact wrote.
func writeResumeBrief(out io.Writer, p Payload) {
	var lines []string
	if p.Cwd != "" {
		lines = append(lines, "- Working directory: "+p.Cwd)
		lines = append(lines, gitState(p.Cwd)...)
	}
	if sessionIDPattern.MatchString(p.SessionID) {
		if rec, err := session.ReadCheckpoint(paths.SessionsDir(), p.SessionID); err == nil {
			if rec.LastRequest != "" {
				lines = append(lines, "- Last request: "+rec.LastRequest)
			}
			if rec.LastReply != "" {
				lines = append(lines, "- Last reply (excerpt): "+rec.LastReply)
			}
		}
	}
	if len(lines) == 0 {
		return
	}
	fmt.Fprintln(out, "## Resume brief (context was just compacted)")
	for _, l := range lines {
		fmt.Fprintln(out, l)
	}
	fmt.Fprintln(out)
}

// gitState describes the checkout at dir, or nothing outside a repository.
func gitState(dir string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*gitProbeTimeout)
	defer cancel()
	branch, err := gitOutput(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || branch == "" {
		return nil
	}
	lines := []string{"- Branch: " + branch}
	if status, err := gitOutput(ctx, dir, "status", "--porcelain"); err == nil {
		var files []string
		for _, l := range strings.Split(status, "\n") {
			if len(l) > 3 {
				files = append(files, strings.TrimSpace(l[3:]))
			}
		}
		switch {
		case len(files) == 0:
			lines = append(lines, "- Uncommitted changes: none")
		case len(files) > maxBriefFiles:
			lines = append(lines, fmt.Sprintf("- Uncommitted changes (%d files): %s, …", len(files), strings.Join(files[:maxBriefFiles], ", ")))
		default:
			lines = append(lines, fmt.Sprintf("- Uncommitted changes (%d files): %s", len(files), strings.Join(files, ", ")))
		}
	}
	if log, err := gitOutput(ctx, dir, "log", "-3", "--format=%h %s"); err == nil && log != "" {
		lines = append(lines, "- Recent commits: "+strings.ReplaceAll(log, "\n", "; "))
	}
	return lines
}
