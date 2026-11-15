package feeds

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/logs"
	"github.com/willesq/orchestrator/internal/rag"
	"github.com/willesq/orchestrator/internal/redact"
)

// Connector reads one chat system. Sync calls Read once per run; the
// connector calls emit for each conversation that changed since it last
// read, and keeps its own watermarks, advancing one only after emit returned
// nil for what it covers. An error from emit means the record was not
// stored: the connector stops and returns it, so the next run reads it again.
type Connector interface {
	System() System
	Read(ctx context.Context, emit func(Record) error) error
}

// Ingester stores text into RAG (implemented by *rag.Service, whose placer
// decides each write's space).
type Ingester interface {
	IngestText(ctx context.Context, text string, m rag.Meta) (int, error)
}

// Allowlist says which conversations are stored. A container is stored only
// when the operator names it; DMs and group DMs only for a tenant whose DMs
// the operator turned on. Tenants are named as a policy binds them,
// "<system>:<id>", and match regardless of case; container IDs match exactly.
type Allowlist struct {
	// DirectMessages lists the tenants whose DMs and group DMs are stored.
	DirectMessages []string
	// Containers lists, per tenant, the channels and meetings stored.
	Containers map[string][]string
}

// Allows reports whether r is stored.
func (a Allowlist) Allows(r Record) bool {
	key := strings.ToLower(r.TenantKey())
	if r.Kind.IsDirectMessage() {
		for _, t := range a.DirectMessages {
			if strings.ToLower(t) == key {
				return true
			}
		}
		return false
	}
	for t, ids := range a.Containers {
		if strings.ToLower(t) != key {
			continue
		}
		for _, id := range ids {
			if id == r.Container {
				return true
			}
		}
	}
	return false
}

// Report counts what a run did. It names no content.
type Report struct {
	// Stored records were written (an unchanged one is skipped by the store).
	Stored int
	// Skipped records are not on the allowlist.
	Skipped int
	// Refused records' facts did not hold, or came from another system
	// than the connector's.
	Refused int
}

// Sync reads c and stores every record allow names through ing. Records
// that are not allowed are skipped, and records whose facts do not hold are
// refused and logged by URI only; neither stops the run. A failed write
// stops it, and its error is returned along with what was done so far.
func Sync(ctx context.Context, c Connector, allow Allowlist, ing Ingester) (Report, error) {
	var rep Report
	err := c.Read(ctx, func(r Record) error {
		if err := r.Validate(); err != nil {
			rep.Refused++
			log.Printf("feeds: %s: refused a record: %s", c.System(), logs.Clean(err.Error()))
			return nil
		}
		if r.System != c.System() {
			rep.Refused++
			log.Printf("feeds: %s: refused a record of system %s", c.System(), logs.Clean(string(r.System)))
			return nil
		}
		if !allow.Allows(r) {
			rep.Skipped++
			return nil
		}
		// Reason: people paste credentials into chats; what is stored is
		// read back into prompts.
		text, _ := redact.Redact(r.Text)
		if _, err := ing.IngestText(ctx, text, r.meta()); err != nil {
			return fmt.Errorf("store %s: %w", logs.Clean(r.URI()), err)
		}
		rep.Stored++
		return nil
	})
	if err != nil {
		return rep, fmt.Errorf("feeds: %s: %w", c.System(), err)
	}
	return rep, nil
}

// AllowlistFrom is the allowlist the operator's chat feed settings name
// (feeds.chat.direct_messages and feeds.chat.containers).
func AllowlistFrom(s config.ChatFeedSettings) Allowlist {
	return Allowlist{DirectMessages: s.DirectMessages, Containers: s.Containers}
}
