package session

import "github.com/willesq/orchestrator/internal/redact"

// ParseRedacted parses a transcript and redacts secrets from every message.
func ParseRedacted(path string) ([]Message, error) {
	msgs, err := ParseTranscript(path)
	if err != nil {
		return nil, err
	}
	for i := range msgs {
		msgs[i].Text, _ = redact.Redact(msgs[i].Text)
	}
	return msgs, nil
}
