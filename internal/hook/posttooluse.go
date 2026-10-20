package hook

import (
	"encoding/json"
	"os"
	"regexp"

	"github.com/willesq/orchestrator/internal/handoff"
)

var ghPRRe = regexp.MustCompile(`\bgh\s+pr\s+(create|ready|close|merge|reopen|edit)\b`)

// postToolUse reacts to tool calls: edited native-config and markdown files are
// stored again, and a `gh pr` mutation refreshes the PR cache. orchd does both;
// the hook only hands them off, so it never holds up the agent.
func postToolUse(p Payload) error {
	switch p.ToolName {
	case "Bash":
		return maybePokeBabysitter(p)
	case "Edit", "Write", "MultiEdit":
		return maybeReingest(p)
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
	// A gh pr mutation happened: ask orchd to refresh the PR cache. Refreshing
	// it here took over ten seconds, all of it spent blocking the agent.
	_ = handoff.RequestPRSweep(handoff.Dir())
	return nil
}

// maybeReingest hands an edited document to orchd to store again; embedding it
// here would hold up the agent for as long as the document takes.
func maybeReingest(p Payload) error {
	var in struct {
		FilePath string `json:"file_path"`
	}
	_ = json.Unmarshal(p.ToolInput, &in)
	home, _ := os.UserHomeDir()
	if _, _, ok := handoff.Classify(in.FilePath, home); !ok {
		return nil
	}
	_ = handoff.PutFile(handoff.Dir(), handoff.File{Path: in.FilePath})
	return nil
}
