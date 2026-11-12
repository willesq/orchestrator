package daemon

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/rag"
	"github.com/willesq/orchestrator/internal/session"
)

// sessionOrigin is what orchd reports about a session's own record or
// transcript: the session and, when known, its first directory (read from its
// record when cwd is ""). The writePlacer places it where the session is
// pinned. A hand-off entry or a summary request names both the session and
// the transcript, and any process of the user can write one, so the session
// is reported only when the transcript is where its harness keeps that
// session's transcript (transcriptOf); otherwise the write is tied to no
// session and is quarantined under a policy.
func sessionOrigin(id, cwd, transcript string) rag.Origin {
	if cwd == "" {
		if rec, err := session.ReadRecord(paths.SessionsDir(), id); err == nil {
			cwd = rec.Cwd
		}
	}
	if !filepath.IsAbs(cwd) {
		cwd = ""
	}
	o := rag.Origin{System: rag.OriginSession, Path: cwd}
	if transcriptOf(id, transcript) {
		o.Session = id
	}
	return o
}

// transcriptRoots are where the harnesses keep transcripts: Claude Code's
// projects folder and Codex's sessions folder, following CLAUDE_CONFIG_DIR
// and CODEX_HOME when orchd has them. A variable so tests can move them.
var transcriptRoots = func() (claude, codex string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ""
	}
	claudeDir, codexDir := filepath.Join(home, ".claude"), filepath.Join(home, ".codex")
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); filepath.IsAbs(d) {
		claudeDir = d
	}
	if d := os.Getenv("CODEX_HOME"); filepath.IsAbs(d) {
		codexDir = d
	}
	return filepath.Join(claudeDir, "projects"), filepath.Join(codexDir, "sessions")
}

// codexRollout is a Codex transcript's file name: rollout-<time>-<id>.jsonl.
// The whole remainder after the time must be the ID, so a fragment of a
// session's UUID does not match.
var codexRollout = regexp.MustCompile(`^rollout-\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}-.+\.jsonl$`)

// transcriptOf reports whether transcript is a regular file where its
// harness keeps session id's transcript: <claude>/projects/<project>/<id>.jsonl
// or <codex>/sessions/.../rollout-<time>-<id>.jsonl. Reason: a file an agent
// names after another session's ID anywhere else would otherwise be stored in
// that session's space; inside the harness's own folders it takes a write
// the OS sandbox can deny (SP-046).
func transcriptOf(id, transcript string) bool {
	if id == "" || !filepath.IsAbs(transcript) || filepath.Clean(transcript) != transcript {
		return false
	}
	claude, codex := transcriptRoots()
	base := filepath.Base(transcript)
	switch {
	case claude != "" && base == id+".jsonl" && filepath.Dir(filepath.Dir(transcript)) == claude:
	case codex != "" && codexRollout.MatchString(base) && strings.TrimSuffix(base[len("rollout-2006-01-02T15-04-05-"):], ".jsonl") == id &&
		strings.HasPrefix(transcript, codex+string(filepath.Separator)):
	default:
		return false
	}
	info, err := os.Lstat(transcript)
	return err == nil && info.Mode().IsRegular()
}
