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

// TrustOf is a chunk's tier: the one its writer stored, lowered to web or
// tool when the chunk's type or URI says it is a web page or tool output.
// Lowering applies to every tier but a syncer's (repo, web), whose type and
// URI orchd chose itself; a client, an edited file or a session that carries
// a page is still that page. Content stored before writers were recorded is
// unverified unless its type or URI lowers it. A declared type or URI can
// only lower a tier, never raise it.
func TrustOf(stored, sourceType, uri string) Trust {
	derived := derivedTrust(sourceType, uri)
	switch t := Trust(strings.TrimSpace(stored)); t {
	case TrustRepo, TrustWeb:
		return t
	case TrustTool:
		return TrustTool
	case TrustSession, TrustFile, TrustClient:
		if derived != "" {
			return derived
		}
		return t
	}
	if derived != "" {
		return derived
	}
	return TrustUnverified
}

// derivedTrust is web or tool when a chunk's type or URI says so, else "".
// The URI is folded (NFKC, lowercased, trimmed) first, so a fullwidth or
// padded scheme is still a scheme.
func derivedTrust(sourceType, uri string) Trust {
	st := strings.ToLower(strings.TrimSpace(sourceType))
	u := strings.ToLower(strings.TrimSpace(norm.NFKC.String(uri)))
	scheme := ""
	if i := strings.Index(u, ":"); i > 0 {
		scheme = u[:i]
	}
	switch {
	case webTypes[st] || strings.Contains(scheme, "http") || strings.Contains(scheme, "ftp") ||
		strings.HasPrefix(u, "www.") || strings.HasPrefix(u, "//"):
		return TrustWeb
	case toolTypes[st]:
		return TrustTool
	}
	return ""
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
	if instructionPatterns.MatchString(text) {
		return true
	}
	// Normalized in overlapping windows, so a payload deep in a long text is
	// still seen, up to maxScan bytes.
	if len(text) > maxScan {
		text = text[:maxScan]
	}
	for start := 0; ; start += maxNormalize / 2 {
		end := min(start+maxNormalize, len(text))
		if instructionPatterns.MatchString(normalize(text[start:end])) {
			return true
		}
		if end == len(text) {
			return false
		}
	}
}

// maxScan bounds how much of a text the normalized check reads; retrieved
// chunks are a few kilobytes.
const maxScan = 1 << 20

// markup is emphasis and quoting that can split an instruction's words.
var markup = regexp.MustCompile("[*_`~>#|]+")

// normalize folds compatibility forms (fullwidth and styled letters),
// removes zero-width and format characters and markdown emphasis, and
// collapses whitespace, so "**ignore** all *previous*\ninstructions" reads
// as the sentence it is.
func normalize(text string) string {
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

// maxNormalize is the window the normalized check reads at a time.
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
