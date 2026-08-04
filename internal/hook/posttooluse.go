package hook

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/prs"
	"github.com/willesq/orchestrator/internal/redact"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

var ghPRRe = regexp.MustCompile(`\bgh\s+pr\s+(create|ready|close|merge|reopen|edit)\b`)

// postToolUse reacts to tool calls: re-ingest edited native-config/markdown files
// into RAG, and poke the PR babysitter after a `gh pr` mutation. Never blocks.
func postToolUse(p Payload, cf clientFactory) error {
	switch p.ToolName {
	case "Bash":
		return maybePokeBabysitter(p)
	case "Edit", "Write", "MultiEdit":
		return maybeReingest(p, cf)
	}
	return nil
}

func maybePokeBabysitter(p Payload) error {
	var in struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(p.ToolInput, &in)
	if !ghPRRe.MatchString(in.Command) {
		return nil
	}
	// A gh pr mutation happened — refresh the PR cache in the background.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = prs.New(paths.PRStateFile(), paths.EventsDir()).Sweep(ctx)
	return nil
}

func maybeReingest(p Payload, cf clientFactory) error {
	var in struct {
		FilePath string `json:"file_path"`
	}
	_ = json.Unmarshal(p.ToolInput, &in)
	if in.FilePath == "" {
		return nil
	}
	sourceType, uri, ok := classifyPath(in.FilePath)
	if !ok {
		return nil
	}
	raw, err := os.ReadFile(in.FilePath)
	if err != nil {
		return nil
	}
	text, redactions := redact.Redact(string(raw))
	if redactions > redact.Threshold {
		return nil
	}

	client, cleanup, err := cf()
	if err != nil {
		return nil
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = client.Ingest(ctx, &orchdv1.IngestRequest{
		Text: text, SourceUri: uri, SourceType: sourceType,
	})
	return nil
}

// classifyPath decides whether an edited file should be re-ingested and how.
func classifyPath(path string) (sourceType, uri string, ok bool) {
	home, _ := os.UserHomeDir()
	claudeDir := home + "/.claude/"
	if home != "" && strings.HasPrefix(path, claudeDir) {
		rel := strings.TrimPrefix(path, claudeDir)
		return "claude_config", "claude://" + rel, true
	}
	if strings.HasSuffix(strings.ToLower(path), ".md") {
		return "repo_doc", "file://" + path, true
	}
	return "", "", false
}
