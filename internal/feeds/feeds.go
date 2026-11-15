// Package feeds is the chat connector framework (SP-060, D-07): a connector
// for a chat system (Slack, Teams) reports each conversation it reads as a
// Record of facts — the system, tenant, container, the container's kind and
// its participants by stable ID — and Sync stores the records the operator's
// allowlist names through orchd's placed write path. A connector never says
// where content goes: orchd's placer puts it in the space its tenant is bound
// to, and an unbound tenant's content in quarantine (INV-03). Direct messages
// are stored, but only an explicit interactive search reads them (rag.ChatDM).
//
// Credentials stay out of orch (see docs/internals/memory.md, "Chat feeds"):
// as with a space's gh profile, the user logs a connector in themselves, and
// orchd hands the connector process only the path of that login's profile
// directory; it never reads inside it or handles a token.
package feeds

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/willesq/orchestrator/internal/rag"
)

// System is the chat system a connector reads.
type System string

// The chat systems. They match spaces.SystemSlack and spaces.SystemTeams,
// and a policy binds their tenants as "<system>:<id>".
const (
	Slack System = rag.OriginSlack
	Teams System = rag.OriginTeams
)

// ContainerKind is the kind of conversation a record comes from.
type ContainerKind string

// The container kinds.
const (
	DM             ContainerKind = rag.ChatDM
	GroupDM        ContainerKind = rag.ChatGroupDM
	PrivateChannel ContainerKind = rag.ChatPrivateChannel
	PublicChannel  ContainerKind = rag.ChatPublicChannel
	Meeting        ContainerKind = rag.ChatMeeting
)

// IsDirectMessage reports whether k is a one-to-one or group DM.
func (k ContainerKind) IsDirectMessage() bool { return rag.IsDirectMessage(string(k)) }

// Record is one conversation as a connector read it: a thread, or a
// container's messages outside any thread, or a meeting's chat. It holds
// facts only. Reason (SP-060.1.a3): a connector cannot name a space, trust
// tier, scope or tag, so nothing it reports can widen where content goes or
// who reads it; there are no such fields to set.
type Record struct {
	System System
	// Tenant is the workspace's or tenant's stable ID, without its system.
	Tenant string
	// Container is the channel's, chat's or meeting's stable ID.
	Container string
	Kind      ContainerKind
	// Thread is the thread's stable ID; "" for the container's messages
	// outside any thread.
	Thread string
	// Participants are the members' stable IDs.
	Participants []string
	// Text is the conversation as the connector rendered it.
	Text string
	// StartedAt and EndedAt span the conversation's messages.
	StartedAt, EndedAt time.Time
}

// SourceType is the source type chat content is stored under.
const SourceType = "chat"

// idPattern is a stable ID a record may hold: Slack's (T0123, C0456,
// 1700000000.000100) and Teams' (GUIDs, 19:abc@thread.v2). It has no '/',
// so a URI's parts cannot run into each other, and no ',', which joins
// participants.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@=+-]{0,255}$`)

// short is v cut to 64 bytes for an error: a value that is not an ID can be
// anything a connector read.
func short(v string) string {
	if len(v) > 64 {
		return strings.ToValidUTF8(v[:64], "") + "…"
	}
	return v
}

// ErrInvalidRecord refuses a record whose facts do not hold.
var ErrInvalidRecord = errors.New("invalid chat record")

// Validate reports whether r's facts hold: a known system and kind, and
// stable IDs where IDs go.
func (r Record) Validate() error {
	if r.System != Slack && r.System != Teams {
		return fmt.Errorf("%w: system %q is not slack or teams", ErrInvalidRecord, short(string(r.System)))
	}
	if !slices.Contains(rag.ChatKinds, string(r.Kind)) {
		return fmt.Errorf("%w: container kind %q is not one of %v", ErrInvalidRecord, short(string(r.Kind)), rag.ChatKinds)
	}
	for _, f := range []struct{ name, v string }{{"tenant", r.Tenant}, {"container", r.Container}} {
		if !idPattern.MatchString(f.v) {
			return fmt.Errorf("%w: %s %q is not a stable ID", ErrInvalidRecord, f.name, short(f.v))
		}
	}
	if r.Thread != "" && !idPattern.MatchString(r.Thread) {
		return fmt.Errorf("%w: thread %q is not a stable ID", ErrInvalidRecord, short(r.Thread))
	}
	for _, p := range r.Participants {
		if !idPattern.MatchString(p) {
			return fmt.Errorf("%w: participant %q is not a stable ID", ErrInvalidRecord, short(p))
		}
	}
	if strings.TrimSpace(r.Text) == "" {
		return fmt.Errorf("%w: it has no text", ErrInvalidRecord)
	}
	if looksDirect(r) && !r.Kind.IsDirectMessage() {
		// Reason: a DM is hidden from every read but the operator's DM
		// search by its kind alone, so a conversation whose ID says it is
		// one is not stored as anything else.
		return fmt.Errorf("%w: container %q is a direct message, not a %s", ErrInvalidRecord, r.Container, r.Kind)
	}
	return nil
}

// looksDirect reports whether r's container ID says it is a direct message
// where the system's IDs say so reliably: a Slack ID starting with D (an
// IM), or a Teams one-to-one chat (…@unq.gbl.spaces).
func looksDirect(r Record) bool {
	switch r.System {
	case Slack:
		return strings.HasPrefix(r.Container, "D")
	case Teams:
		return strings.HasSuffix(strings.ToLower(r.Container), "@unq.gbl.spaces")
	}
	return false
}

// TenantKey is r's tenant as a policy binds it: "<system>:<tenant>".
func (r Record) TenantKey() string { return string(r.System) + ":" + r.Tenant }

// URI is the source URI r is stored under:
// <system>://<tenant>/<container>[/<thread>], e.g.
// slack://T0123/C0456/1700000000.000100. A thread that grows is written
// again under the same URI.
func (r Record) URI() string {
	u := string(r.System) + "://" + r.Tenant + "/" + r.Container
	if r.Thread != "" {
		u += "/" + r.Thread
	}
	return u
}

// meta is how r is stored: only facts, which orchd's placer reads to decide
// its space. Space is left empty; the placer alone sets it.
func (r Record) meta() rag.Meta {
	parts := slices.Clone(r.Participants)
	slices.Sort(parts)
	parts = slices.Compact(parts)
	return rag.Meta{
		SourceURI:  r.URI(),
		SourceType: SourceType,
		Writer:     rag.WriterChat,
		StartedAt:  r.StartedAt,
		EndedAt:    r.EndedAt,
		Origin: rag.Origin{
			System:        string(r.System),
			Tenant:        r.Tenant,
			Container:     r.Container,
			ContainerKind: string(r.Kind),
			Participants:  strings.Join(parts, ","),
		},
	}
}
