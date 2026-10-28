// Package memctx renders retrieved memory for an agent's prompt: compact
// pointers (or full text) for auto-injection, each labelled with how far its
// origin can be trusted, with instruction-like text from untrusted origins
// shown only as quoted data.
package memctx

import (
	"regexp"
	"strings"
	"unicode"
)

// Trust is how far a memory's origin can be trusted.
type Trust string

// Trust tiers, most trusted first.
const (
	// TrustSession is the user's own sessions: their records, summaries and
	// transcripts.
	TrustSession Trust = "session"
	// TrustRepo is documentation from the user's repositories and config.
	TrustRepo Trust = "repo"
	// TrustNote is a note an agent or the user stored by hand.
	TrustNote Trust = "note"
	// TrustWeb is a fetched web page: third-party text.
	TrustWeb Trust = "web"
	// TrustTool is captured tool output, such as PR or issue bodies.
	TrustTool Trust = "tool"
)

var (
	sessionTypes = map[string]bool{"session_record": true, "session_transcript": true, "session_summary": true}
	repoTypes    = map[string]bool{"repo_doc": true, "claude_config": true, "file": true}
	webTypes     = map[string]bool{"web": true, "webpage": true, "url": true}
	toolTypes    = map[string]bool{"tool_output": true, "tool": true, "github": true, "pr": true, "issue": true}
)

// TrustOf derives a chunk's trust from its source type and URI. The URI's
// scheme must agree with a trusted type: a "repo_doc" stored under an
// https:// URI is still a web page. Unknown types are notes.
//
// Both fields are written by whoever stored the chunk, so this ranks honest
// writers' content; it cannot unmask a writer that lies about both.
func TrustOf(sourceType, uri string) Trust {
	st := strings.ToLower(strings.TrimSpace(sourceType))
	scheme := ""
	if i := strings.Index(uri, "://"); i > 0 {
		scheme = strings.ToLower(uri[:i])
	}
	switch {
	case webTypes[st] || scheme == "http" || scheme == "https":
		return TrustWeb
	case toolTypes[st]:
		return TrustTool
	case sessionTypes[st] && scheme == "session":
		return TrustSession
	case repoTypes[st] && (scheme == "repo" || scheme == "file" || scheme == "claude"):
		return TrustRepo
	}
	return TrustNote
}

// Untrusted reports whether content of this tier is third-party text: it is
// labelled untrusted, and prompt context can leave it out.
func (t Trust) Untrusted() bool { return t == TrustWeb || t == TrustTool }

// Fenced reports whether this tier's text is shown only as quoted data, with
// instruction-like text flagged: everything but the user's own sessions and
// repo docs. A note may hold text an agent copied from anywhere.
func (t Trust) Fenced() bool { return t != TrustSession && t != TrustRepo }

// Label names the tier for a reader.
func (t Trust) Label() string {
	switch t {
	case TrustSession:
		return "own session"
	case TrustRepo:
		return "repo doc"
	case TrustWeb:
		return "web page, untrusted"
	case TrustTool:
		return "tool output, untrusted"
	}
	return "note"
}

// instructionPatterns catch text addressed to an agent rather than about a
// subject: overrides of earlier instructions, role claims, and commands to
// run, fetch or exfiltrate something.
var instructionPatterns = regexp.MustCompile(`(?i)` + strings.Join([]string{
	`\b(ignore|disregard|forget|override)\b[^.\n]{0,40}\b(previous|prior|above|earlier|all|any|your|system)\b[^.\n]{0,20}\b(instructions?|prompts?|rules|directions|guidelines|messages?)`,
	`\byou (must|should|need to|are required to|have to)\s+(now\s+)?(run|execute|call|curl|wget|fetch|send|post|upload|delete|remove|install|open|visit|download|ignore|disable|approve|merge|push)\b`,
	`\b(new|updated|real|actual|hidden)\s+(system\s+)?instructions?\b`,
	`\b(system|developer)\s*(prompt|message)\s*:`,
	`\byou are now\b`,
	`\bdo not (tell|inform|mention|reveal)\b[^.\n]{0,30}\b(user|human|operator)\b`,
	`\b(run|execute)\s+(this|the following)\s+(command|script|code)\b`,
	`(?m)^\s*(assistant|system)\s*:`,
	`\b(curl|wget)\b[^|\n]{0,200}https?://[^|\n]*\|\s*(sudo\s+)?(ba|z)?sh\b`,
	`\b(exfiltrate|send|post|upload)\b[^.\n]{0,40}\b(token|secret|credential|password|api key|ssh key|\.env)\b`,
}, "|"))

// InstructionLike reports whether text reads like an instruction to an agent
// rather than information, the shape a prompt-injection payload takes. It is
// a pattern list that rewording, homoglyphs or another language get past, so
// it only adds a warning; it must never be what decides whether content is
// shown or trusted.
func InstructionLike(text string) bool { return instructionPatterns.MatchString(text) }

// Printable replaces control and format characters (bidi overrides,
// zero-width and Unicode tag characters, which can hide text from a reader)
// with '?', keeping newlines and tabs, and makes s valid UTF-8.
func Printable(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || (r >= 0xE0000 && r <= 0xE007F):
			return '?'
		}
		return r
	}, strings.ToValidUTF8(s, "?"))
}
