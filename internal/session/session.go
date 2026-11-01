// Package session parses harness transcript JSONL into a compact record and text
// suitable for RAG ingestion + on-disk session records. It parses leniently:
// malformed lines are skipped rather than failing the whole transcript, because
// the hooks that call it must never break a session.
package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/willesq/orchestrator/internal/timeline"
)

// Message is one extracted turn.
type Message struct {
	Role string
	Text string
	// Time is when the harness logged the turn; zero when the transcript does
	// not say.
	Time time.Time
}

// Record is the compact, human-inspectable per-session record.
type Record struct {
	SessionID string `yaml:"session_id"`
	Cwd       string `yaml:"cwd,omitempty"`
	// StartedAt and EndedAt are the session's first and last turns, RFC 3339
	// in UTC; empty when the transcript carries no times.
	StartedAt      string `yaml:"started_at,omitempty"`
	EndedAt        string `yaml:"ended_at,omitempty"`
	Summary        string `yaml:"summary"`
	TranscriptPath string `yaml:"transcript_path,omitempty"`
	Messages       int    `yaml:"messages"`
	// LastRequest and LastReply are the final user request and assistant reply,
	// kept on mid-session checkpoints so a compacted session can be reminded
	// where it was.
	LastRequest string `yaml:"last_request,omitempty"`
	LastReply   string `yaml:"last_reply,omitempty"`
}

type rawLine struct {
	Type      string          `json:"type"`
	Message   json.RawMessage `json:"message"`
	Timestamp string          `json:"timestamp"`
	// IsMeta and IsCompactSummary mark turns the harness wrote in the user's name.
	IsMeta           bool `json:"isMeta"`
	IsCompactSummary bool `json:"isCompactSummary"`
}

// spokenLine reports whether a transcript line is a turn someone said. Harnesses
// also log what they sent the model: Claude Code's api-request-blob lines carry
// a message holding the whole context (instruction files, skill and tool lists,
// earlier hook output) with the role "user". Stored as turns, those made up more
// than half of a transcript's text and matched each other across sessions.
func spokenLine(rl rawLine) bool {
	if rl.IsMeta || rl.IsCompactSummary {
		return false
	}
	switch rl.Type {
	case "", "user", "assistant", "message":
		return true
	}
	return false
}

// harnessTags are the blocks a harness puts inside a user turn.
var harnessTags = []string{"system-reminder", "task-notification", "agent-message", "local-command-stdout", "local-command-stderr", "local-command-caveat"}

// stripHarnessText removes harness-written blocks from a user's turn, each from
// its opening tag to its own closing tag, leaving a tag that is never closed alone.
func stripHarnessText(text string) string {
	for _, tag := range harnessTags {
		text = cutBlocks(text, tag)
	}
	// The notice a harness prints above a background-task event is the whole turn.
	if strings.HasPrefix(strings.TrimSpace(text), "[SYSTEM NOTIFICATION - NOT USER INPUT]") {
		return ""
	}
	return text
}

// cutBlocks removes <tag ...>...</tag> spans in one linear pass.
func cutBlocks(text, tag string) string {
	open, closer := "<"+tag, "</"+tag+">"
	if !strings.Contains(text, open) {
		return text
	}
	var b strings.Builder
	for {
		i := strings.Index(text, open)
		if i < 0 {
			break
		}
		// "<system-reminder-x" is some other tag.
		rest := text[i+len(open):]
		if rest == "" || !strings.ContainsRune("> \t\n", rune(rest[0])) {
			b.WriteString(text[:i+len(open)])
			text = rest
			continue
		}
		end := strings.Index(rest, closer)
		if end < 0 {
			break
		}
		b.WriteString(text[:i])
		text = rest[end+len(closer):]
	}
	b.WriteString(text)
	return b.String()
}

type rawMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ParseTranscript reads a transcript JSONL file and extracts the text of the
// user and assistant turns, leaving out what the harness itself wrote.
func ParseTranscript(path string) ([]Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var msgs []Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var rl rawLine
		if json.Unmarshal(line, &rl) != nil || len(rl.Message) == 0 || !spokenLine(rl) {
			continue
		}
		var rm rawMessage
		if json.Unmarshal(rl.Message, &rm) != nil {
			continue
		}
		role := rm.Role
		if role == "" {
			role = rl.Type
		}
		text := extractText(rm.Content)
		// Only a user's turn carries harness blocks; an assistant may be discussing them.
		if role == "user" {
			text = stripHarnessText(text)
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		// A missing or malformed time leaves the turn undated, not dropped.
		ts, _ := time.Parse(time.RFC3339Nano, rl.Timestamp)
		msgs = append(msgs, Message{Role: role, Text: text, Time: ts.UTC()})
	}
	return msgs, sc.Err()
}

// extractText handles content that is either a JSON string or an array of blocks.
func extractText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var blocks []contentBlock
	if json.Unmarshal(content, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// TranscriptText renders messages as a single "role: text" document for ingestion.
func TranscriptText(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		// Each turn carries its own time, so a session spanning days says which
		// day each request was made, and each stored chunk can be dated.
		if !m.Time.IsZero() {
			b.WriteString(timeline.Marker(m.Time))
			b.WriteString(" ")
		}
		b.WriteString(m.Role)
		b.WriteString(": ")
		// A message quoting a marker must not date the chunk it lands in.
		b.WriteString(timeline.Neutralize(m.Text))
		b.WriteString("\n\n")
	}
	return b.String()
}

// Summarize builds a short summary from the assistant's contributions.
func Summarize(msgs []Message, maxChars int) string {
	if maxChars <= 0 {
		maxChars = 280
	}
	var parts []string
	for _, m := range msgs {
		if m.Role == "assistant" {
			parts = append(parts, strings.TrimSpace(m.Text))
		}
	}
	joined := strings.Join(parts, " ")
	joined = strings.Join(strings.Fields(joined), " ") // collapse whitespace
	if joined == "" && len(msgs) > 0 {
		joined = strings.Join(strings.Fields(msgs[0].Text), " ")
	}
	if len(joined) > maxChars {
		joined = joined[:maxChars] + "…"
	}
	return joined
}

// BuildRecord assembles a Record from parsed messages.
func BuildRecord(sessionID, cwd, transcriptPath string, msgs []Message) *Record {
	r := &Record{
		SessionID:      sessionID,
		Cwd:            cwd,
		Summary:        Summarize(msgs, 280),
		TranscriptPath: transcriptPath,
		Messages:       len(msgs),
	}
	r.SetSpan(Span(msgs))
	return r
}

// Span returns the times of the first and last dated turns; zero when none is.
func Span(msgs []Message) (start, end time.Time) {
	for _, m := range msgs {
		if m.Time.IsZero() {
			continue
		}
		if start.IsZero() || m.Time.Before(start) {
			start = m.Time
		}
		if m.Time.After(end) {
			end = m.Time
		}
	}
	return start, end
}

// SetSpan records when the session ran; zero times leave the fields empty.
func (r *Record) SetSpan(start, end time.Time) {
	if !start.IsZero() {
		r.StartedAt = start.UTC().Format(time.RFC3339)
	}
	if !end.IsZero() {
		r.EndedAt = end.UTC().Format(time.RFC3339)
	}
}

// Span returns when the session ran; zero for a time the record lacks.
func (r *Record) Span() (start, end time.Time) {
	start, _ = time.Parse(time.RFC3339, r.StartedAt)
	end, _ = time.Parse(time.RFC3339, r.EndedAt)
	return start, end
}

// Text is the record as stored: its summary, led by when the session ran so
// the stored text dates itself.
func (r *Record) Text() string {
	start, end := r.Span()
	if start.IsZero() && end.IsZero() {
		return r.Summary
	}
	return "Session " + timeline.Range(start, end) + "\n\n" + r.Summary
}

// WriteRecord writes the authoritative record to <dir>/<id>.yaml.
func WriteRecord(dir string, r *Record) error {
	path, err := sessionFile(dir, r.SessionID, ".yaml")
	if err != nil {
		return err
	}
	return writeYAML(path, r)
}

// ReadRecord loads a session's authoritative record.
func ReadRecord(dir, sessionID string) (*Record, error) {
	b, err := os.ReadFile(filepath.Join(dir, sessionID+".yaml"))
	if err != nil {
		return nil, err
	}
	var r Record
	if err := yaml.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// WriteCheckpoint writes a mid-session checkpoint to <dir>/<id>.checkpoint.yaml.
func WriteCheckpoint(dir string, r *Record) error {
	path, err := sessionFile(dir, r.SessionID, ".checkpoint.yaml")
	if err != nil {
		return err
	}
	return writeYAML(path, r)
}

// ReadCheckpoint loads a session's mid-session checkpoint.
func ReadCheckpoint(dir, sessionID string) (*Record, error) {
	b, err := os.ReadFile(filepath.Join(dir, sessionID+".checkpoint.yaml"))
	if err != nil {
		return nil, err
	}
	var r Record
	if err := yaml.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// maxTurnChars bounds each half of the last turn kept on a checkpoint.
const maxTurnChars = 600

// LastTurn returns the most recent user request and assistant reply. Harness
// bookkeeping that arrives as user messages (slash-command echoes, local
// command output) is not a request and is passed over.
func LastTurn(msgs []Message) (request, reply string) {
	for i := len(msgs) - 1; i >= 0 && (request == "" || reply == ""); i-- {
		text := strings.Join(strings.Fields(msgs[i].Text), " ")
		switch {
		case msgs[i].Role == "assistant" && reply == "":
			reply = clip(text, maxTurnChars)
		case msgs[i].Role == "user" && request == "" && !strings.HasPrefix(text, "<"):
			request = clip(text, maxTurnChars)
		}
	}
	return request, reply
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// DeleteCheckpoint removes a session's checkpoint if present.
func DeleteCheckpoint(dir, sessionID string) error {
	path, err := sessionFile(dir, sessionID, ".checkpoint.yaml")
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// ErrInvalidSessionID rejects a session ID that cannot safely name a file.
var ErrInvalidSessionID = errors.New("invalid session id")

// idPattern is the shape of a harness session ID. IDs arrive in hook payloads,
// so they are checked before naming a file rather than trusted with a path.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// ValidID reports whether id can name a session file.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// sessionFile is <dir>/<id><suffix>, for a valid id only.
func sessionFile(dir, id, suffix string) (string, error) {
	if !ValidID(id) {
		return "", fmt.Errorf("%w: %q", ErrInvalidSessionID, id)
	}
	return filepath.Join(dir, id+suffix), nil
}

func writeYAML(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
