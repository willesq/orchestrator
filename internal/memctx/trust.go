// Package memctx renders retrieved memory for an agent's prompt: compact
// pointers (or full text) for auto-injection, each labelled with the trust
// tier orchd's writer gave it. Retrieved text is always shown as quoted data,
// and instruction-like text is flagged in every tier.
package memctx

import (
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Trust is how far a memory's origin can be trusted.
type Trust string

// Trust tiers. The first five are assigned by orchd's writer and stored with
// each chunk (see rag.TrustFor); a client cannot choose them.
const (
	// TrustRepo is a registered repo's docs, stored by the docs or sources sync.
	TrustRepo Trust = "repo"
	// TrustSession is the user's sessions as orchd stored them. They carry the
	// tool output and web pages the agent read, so they are fenced.
	TrustSession Trust = "session"
	// TrustFile is a document an agent edited, stored by the edit hand-off.
	TrustFile Trust = "file"
	// TrustClient is anything stored through the Ingest RPC or MCP
	// rag_ingest_text, whatever type or URI it declared.
	TrustClient Trust = "client"
	// TrustWeb is a fetched web page: third-party text.
	TrustWeb Trust = "web"
	// TrustTool is captured tool output (only derived, for older content).
	TrustTool Trust = "tool"
	// TrustUnverified is content stored before writers were recorded, whose
	// tier cannot be known.
	TrustUnverified Trust = "unverified"
)

var (
	webTypes  = map[string]bool{"web": true, "webpage": true, "url": true}
	toolTypes = map[string]bool{"tool_output": true, "tool": true, "github": true, "pr": true, "issue": true}
)

// TrustOf is a chunk's tier: the one its writer stored when there is one.
// Content stored before writers were recorded is unverified, or web or tool
// output when its type or URI says so; its type and URI were the writer's
// choice, so they can only lower the tier, never raise it.
func TrustOf(stored, sourceType, uri string) Trust {
	switch t := Trust(strings.TrimSpace(stored)); t {
	case TrustRepo, TrustSession, TrustFile, TrustClient, TrustWeb:
		return t
	}
	st := strings.ToLower(strings.TrimSpace(sourceType))
	u := strings.ToLower(strings.TrimSpace(uri))
	scheme := ""
	if i := strings.Index(u, "://"); i > 0 {
		scheme = u[:i]
	}
	switch {
	case webTypes[st] || strings.Contains(scheme, "http") || strings.HasPrefix(u, "www."):
		return TrustWeb
	case toolTypes[st]:
		return TrustTool
	}
	return TrustUnverified
}

// Untrusted reports whether content of this tier is third-party text: it is
// labelled untrusted, and prompt context can leave it out.
func (t Trust) Untrusted() bool { return t == TrustWeb || t == TrustTool }

// Label names the tier for a reader.
func (t Trust) Label() string {
	switch t {
	case TrustRepo:
		return "repo doc"
	case TrustSession:
		return "own session"
	case TrustFile:
		return "edited file"
	case TrustClient:
		return "stored by a client"
	case TrustWeb:
		return "web page, untrusted"
	case TrustTool:
		return "tool output, untrusted"
	}
	return "unverified"
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
func InstructionLike(text string) bool {
	return instructionPatterns.MatchString(text) || instructionPatterns.MatchString(normalize(text))
}

// markup is emphasis and quoting that can split an instruction's words.
var markup = regexp.MustCompile("[*_`~>#|]+")

// normalize folds compatibility forms (fullwidth and styled letters),
// removes zero-width and format characters and markdown emphasis, and
// collapses whitespace, so "**ignore** all *previous*\ninstructions" reads
// as the sentence it is.
func normalize(text string) string {
	if len(text) > maxNormalize {
		text = text[:maxNormalize]
	}
	t := norm.NFKC.String(text)
	t = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, t)
	t = markup.ReplaceAllString(t, "")
	return strings.Join(strings.Fields(t), " ")
}

// maxNormalize bounds the text normalized for matching.
const maxNormalize = 64 << 10

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
		case r == '\u2028' || r == '\u2029':
			// Line and paragraph separators can read as line breaks.
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, "?"))
}
