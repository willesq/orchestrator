// Package hook implements the single binary all harness lifecycle hooks invoke.
// The install registers one command per event: `orch-hook <event>`, and Run
// dispatches by that event slug, reading the harness payload from stdin.
//
// Crash-safety contract (matches the Python system): a hook must NEVER block or
// fail the user's session. Every handler degrades to a no-op on any error and
// returns nil; problems go to stderr, never stdout (stdout is context injection).
package hook

import (
	"encoding/json"
	"io"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/orchdclient"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// Payload is the decoded hook event from the harness (superset across events).
type Payload struct {
	SessionID       string          `json:"session_id"`
	ParentSessionID string          `json:"parent_session_id"`
	Prompt          string          `json:"prompt"`
	TranscriptPath  string          `json:"transcript_path"`
	Cwd             string          `json:"cwd"`
	SubagentType    string          `json:"subagent_type"`
	FinalResponse   string          `json:"final_response"`
	ToolName        string          `json:"tool_name"`
	ToolInput       json.RawMessage `json:"tool_input"`
	// Source says why a SessionStart fired: startup, resume, clear or compact.
	Source string `json:"source"`
}

// clientFactory dials orchd; injected so handlers are testable without a daemon.
type clientFactory func() (orchdv1.OrchdClient, func(), error)

// Run dispatches the hook event named by args[0], reading the payload from in and
// writing any injected context to out. It never returns a session-blocking error.
func Run(args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 {
		return nil
	}
	data, err := io.ReadAll(in)
	if err != nil {
		return nil
	}
	var p Payload
	if len(data) > 0 {
		_ = json.Unmarshal(data, &p)
	}
	return dispatch(args[0], p, out, orchdclient.Dial)
}

func dispatch(event string, p Payload, out io.Writer, cf clientFactory) error {
	switch event {
	case "user-prompt-submit":
		return userPromptSubmit(loadAutoRAG(), p, out, cf)
	case "session-start":
		return sessionStart(p, out)
	case "stop":
		return stop(p)
	case "session-end":
		return sessionEnd(p, cf)
	case "pre-compact":
		return preCompact(p)
	case "subagent-stop":
		return subagentStop(p)
	case "post-tool-use":
		return postToolUse(p)
	default:
		return nil
	}
}

func loadAutoRAG() config.AutoRAGSettings {
	v, err := config.Load()
	if err != nil {
		return config.AutoRAGSettings{}
	}
	s, err := config.Unmarshal(v)
	if err != nil {
		return config.AutoRAGSettings{}
	}
	return s.Hooks.AutoRAG
}
