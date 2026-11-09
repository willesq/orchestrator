// Package toollog is orchd's command log (spaces plan SP-014): every tool
// event the pre-tool-use and cwd-changed hooks hand off, stored in an
// owner-only SQLite file apart from orch.db (D-29), hash-chained per session
// or task. Nothing here is ever indexed into RAG (INV-12). See
// docs/internals/daemon.md "Command log".
package toollog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/willesq/orchestrator/internal/redact"
)

// Record kinds.
const (
	// KindEvent is one tool event.
	KindEvent = "event"
	// KindGap says Dropped events of this owner were lost before they could
	// be stored (D-30).
	KindGap = "gap"
	// KindEnd is written at SessionEnd; its hash is the anchored chain head.
	KindEnd = "end"
)

// Owner kinds.
const (
	OwnerSession = "session"
	OwnerTask    = "task"
)

// MaxCommand caps the stored command, marker included (D-17).
const MaxCommand = 2048

// MaxField caps every other stored text field (RC-29).
const MaxField = 4096

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Owner is whose chain a record belongs to: a pinned session or a queued
// task, as orchd resolved the caller. It is never taken from a request.
type Owner struct {
	Kind string
	ID   string
}

// String is "task:<id>" for a task and the bare ID for a session, the form
// ParseOwner reads.
func (o Owner) String() string {
	if o.Kind == OwnerTask {
		return OwnerTask + ":" + o.ID
	}
	return o.ID
}

// ParseOwner reads "<session id>", "session:<id>" or "task:<id>".
func ParseOwner(s string) (Owner, error) {
	o := Owner{Kind: OwnerSession, ID: s}
	if k, id, ok := strings.Cut(s, ":"); ok {
		o = Owner{Kind: k, ID: id}
	}
	if o.Kind != OwnerSession && o.Kind != OwnerTask {
		return Owner{}, fmt.Errorf("unknown log owner %q: use a session ID or task:<id>", o.Kind)
	}
	if !idPattern.MatchString(o.ID) {
		return Owner{}, fmt.Errorf("IDs are 1 to 128 letters, digits, '-' or '_'")
	}
	return o, nil
}

// Record is one entry of an owner's chain. Every field but Hash is covered
// by Hash, so changing, removing or moving a record breaks the chain.
type Record struct {
	OwnerKind string `json:"owner_kind"`
	OwnerID   string `json:"owner_id"`
	Seq       int64  `json:"seq"`
	// TS is when orchd took the event, unix nanoseconds; HookAt is when the
	// hook started, as the hook reported it.
	TS     int64  `json:"ts"`
	HookAt int64  `json:"hook_at"`
	Kind   string `json:"kind"`

	Harness string `json:"harness"`
	Event   string `json:"event"`
	Tool    string `json:"tool"`
	// ClaimedSession is the session_id the hook sent; it may differ from
	// OwnerID when orchd could not confirm it.
	ClaimedSession string `json:"claimed_session"`
	ToolUseID      string `json:"tool_use_id"`
	AgentID        string `json:"agent_id"`
	AgentType      string `json:"agent_type"`
	Cwd            string `json:"cwd"`
	OldCwd         string `json:"old_cwd"`
	NewCwd         string `json:"new_cwd"`
	// Targets is the JSON of spaces.ToolTargets for the call.
	Targets   string `json:"targets"`
	Command   string `json:"command"`
	InputSize int64  `json:"input_size"`

	Decision  string `json:"decision"`
	Rule      string `json:"rule"`
	PolicyRev int64  `json:"policy_rev"`

	Dropped  int64  `json:"dropped"`
	PrevHash string `json:"prev_hash"`
	Hash     string `json:"-"`
}

// computeHash is the SHA-256 of the record's JSON without Hash. PrevHash is
// part of it, which chains the records.
func (r Record) computeHash() string {
	b, err := json.Marshal(r)
	if err != nil {
		// A struct of strings and integers always marshals.
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// redactMargin is how much text past a field's cap is still redacted, so a
// secret the cap cuts through is found whole.
const redactMargin = 4096

// Clean redacts credentials in s and caps it at max bytes, marker included.
// Text is made valid UTF-8 first: the record's hash is taken over its JSON,
// which would otherwise map different invalid bytes to the same text.
// Redaction (redact.RedactCommand, which also removes a private key whose
// END line the cut lost) runs on a little more than is kept, so a secret cut
// by the cap is still found, and never on much more, so a huge field costs
// little.
func Clean(s string, max int) string {
	size := len(s)
	long := len(s) > 2*max+redactMargin
	if long {
		s = s[:2*max+redactMargin]
	}
	s = strings.ToValidUTF8(s, "\uFFFD")
	s, _ = redact.RedactCommand(s)
	if !long && len(s) <= max {
		return s
	}
	return truncate(s, max, size)
}

// truncate cuts s to max bytes, marker included; the marker names size,
// the original length.
func truncate(s string, max, size int) string {
	marker := fmt.Sprintf("…[truncated %d bytes]", size)
	keep := min(max-len(marker), len(s))
	if keep < 0 {
		keep = 0
	}
	for keep > 0 && keep < len(s) && !utf8.RuneStart(s[keep]) {
		keep--
	}
	return s[:keep] + marker
}

// shellTools name the harness tools that run a shell command, as
// spaces.Targets reads them.
var shellTools = map[string]string{
	"Bash": "command", "shell": "command", "local_shell": "command",
	"container.exec": "command", "exec_command": "cmd",
}

// Command is the text the log keeps for a tool call: a shell tool's command
// (an argv list is joined with spaces), or the tool input itself for any
// other tool. It is credential-redacted and capped at MaxCommand.
func Command(tool string, input []byte) string {
	if field, ok := shellTools[tool]; ok {
		var obj map[string]json.RawMessage
		if json.Unmarshal(input, &obj) == nil {
			var s string
			var argv []string
			switch {
			case json.Unmarshal(obj[field], &s) == nil:
				return Clean(s, MaxCommand)
			case json.Unmarshal(obj[field], &argv) == nil:
				return Clean(strings.Join(argv, " "), MaxCommand)
			}
		}
	}
	return Clean(string(input), MaxCommand)
}
