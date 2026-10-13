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

	"gopkg.in/yaml.v3"
)

// Message is one extracted turn.
type Message struct {
	Role string
	Text string
}

// Record is the compact, human-inspectable per-session record.
type Record struct {
	SessionID      string `yaml:"session_id"`
	Cwd            string `yaml:"cwd,omitempty"`
	EndedAt        string `yaml:"ended_at,omitempty"`
	Summary        string `yaml:"summary"`
	TranscriptPath string `yaml:"transcript_path,omitempty"`
	Messages       int    `yaml:"messages"`
}

type rawLine struct {
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message"`
}

type rawMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ParseTranscript reads a transcript JSONL file and extracts user/assistant text.
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
		if json.Unmarshal(line, &rl) != nil || len(rl.Message) == 0 {
			continue
		}
		var rm rawMessage
		if json.Unmarshal(rl.Message, &rm) != nil {
			continue
		}
		text := extractText(rm.Content)
		if strings.TrimSpace(text) == "" {
			continue
		}
		role := rm.Role
		if role == "" {
			role = rl.Type
		}
		msgs = append(msgs, Message{Role: role, Text: text})
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
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(m.Text)
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
	return &Record{
		SessionID:      sessionID,
		Cwd:            cwd,
		Summary:        Summarize(msgs, 280),
		TranscriptPath: transcriptPath,
		Messages:       len(msgs),
	}
}

// WriteRecord writes the authoritative record to <dir>/<id>.yaml.
func WriteRecord(dir string, r *Record) error {
	path, err := sessionFile(dir, r.SessionID, ".yaml")
	if err != nil {
		return err
	}
	return writeYAML(path, r)
}

// WriteCheckpoint writes a mid-session checkpoint to <dir>/<id>.checkpoint.yaml.
func WriteCheckpoint(dir string, r *Record) error {
	path, err := sessionFile(dir, r.SessionID, ".checkpoint.yaml")
	if err != nil {
		return err
	}
	return writeYAML(path, r)
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
