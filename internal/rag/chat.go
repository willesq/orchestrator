package rag

import (
	"fmt"
	"slices"
)

// Chat content (SP-060, D-07, D-19). A chat feed reports where a message
// came from (its tenant, container and the container's kind) as Origin
// facts; orchd's placer puts it in the space its tenant is bound to. Direct
// messages are stored and found by an explicit interactive search
// (SearchOpts.DirectMessages), but every other read leaves them out: they are
// never recalled into a prompt automatically and never reach a queued task.

// Origin systems of chat content. They match spaces.SystemSlack and
// spaces.SystemTeams.
const (
	OriginSlack = "slack"
	OriginTeams = "teams"
)

// WriterChat is a chat feed's sync (SP-061, SP-062): only orchd's own feed
// code sets it.
const WriterChat = "chat"

// TrustChat is the tier of chat content: what people wrote in a chat, which
// can quote anything.
const TrustChat = "chat"

// Chat container kinds, as Origin.ContainerKind holds them.
const (
	ChatDM             = "dm"
	ChatGroupDM        = "group_dm"
	ChatPrivateChannel = "private_channel"
	ChatPublicChannel  = "public_channel"
	ChatMeeting        = "meeting"
)

// ChatKinds lists every container kind a chat origin may name.
var ChatKinds = []string{ChatDM, ChatGroupDM, ChatPrivateChannel, ChatPublicChannel, ChatMeeting}

// directMessageKinds are the kinds every read but an explicit interactive
// search leaves out.
var directMessageKinds = []string{ChatDM, ChatGroupDM}

// fieldChatKind is the payload field holding a chat chunk's container kind,
// at the top level so a read can filter on it.
const fieldChatKind = "chat_kind"

// excludeDirectMessages is a key of a read's exact-match conditions that
// buildFilter turns into the filter leaving direct messages out. It cannot
// be a payload field name: it starts with a NUL byte.
const excludeDirectMessages = "\x00exclude_direct_messages"

// IsChatOrigin reports whether system is a chat feed's.
func IsChatOrigin(system string) bool {
	return system == OriginSlack || system == OriginTeams
}

// IsDirectMessage reports whether a chat container of kind is a direct
// message: a one-to-one or group DM.
func IsDirectMessage(kind string) bool {
	return slices.Contains(directMessageKinds, kind)
}

// IsSyncWriter reports whether writer is one of orchd's syncs, which keep a
// source up to date themselves: a client may not replace what they store.
func IsSyncWriter(writer string) bool {
	return writer == WriterDocs || writer == WriterSources || writer == WriterChat
}

// checkChatOrigin refuses a chat write whose container kind is not one
// orch knows. Reason: a kind it does not know could be a direct message
// under another name, and only the known DM kinds are kept out of reads.
func checkChatOrigin(o Origin, writer string) error {
	if !IsChatOrigin(o.System) {
		return nil
	}
	if writer != WriterChat {
		return fmt.Errorf("a %s origin is only a chat feed's, not writer %q's", o.System, writer)
	}
	if !slices.Contains(ChatKinds, o.ContainerKind) {
		return fmt.Errorf("a %s write names container kind %q, which is not one of %v", o.System, o.ContainerKind, ChatKinds)
	}
	return nil
}

// withDirectMessages has every store in stores keep direct messages in what
// it reads.
func withDirectMessages(stores []spaceStore, on bool) []spaceStore {
	if !on {
		return stores
	}
	for i := range stores {
		stores[i].directMessages = true
	}
	return stores
}

// directMessageFilter is the must_not condition leaving direct messages out.
func directMessageFilter() map[string]any {
	kinds := make([]any, len(directMessageKinds))
	for i, k := range directMessageKinds {
		kinds[i] = k
	}
	return map[string]any{"key": fieldChatKind, "match": map[string]any{"any": kinds}}
}
