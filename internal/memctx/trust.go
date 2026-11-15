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

// Trust tiers. The first five, and chat, are assigned by orchd's writer and stored with
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
	// TrustChat is what people wrote in a chat a chat feed stored (SP-060):
	// third-party text, guests' included.
	TrustChat Trust = "chat"
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
	case TrustRepo, TrustWeb, TrustChat:
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

// webSchemes are further schemes whose content comes from off the machine
// (or runs as script), read as web pages; a UNC path (\\host\share) too.
var webSchemes = map[string]bool{"ws": true, "wss": true, "data": true, "javascript": true, "smb": true, "gopher": true}

// derivedTrust is web or tool when a chunk's type or URI says so, else "".
// The URI is folded (NFKC, lowercased, whitespace and format characters
// removed) first, so a fullwidth, padded or split scheme is still a scheme.
func derivedTrust(sourceType, uri string) Trust {
	st := strings.ToLower(strings.TrimSpace(sourceType))
	// Format characters and whitespace are dropped first, so " h\u200bttps:"
	// cannot hide a web scheme.
	u := strings.ToLower(strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, norm.NFKC.String(uri)))
	scheme := ""
	if i := strings.Index(u, ":"); i > 0 {
		scheme = u[:i]
	}
	switch {
	case webTypes[st] || strings.Contains(scheme, "http") || strings.Contains(scheme, "ftp") ||
		webSchemes[scheme] || strings.HasPrefix(u, "www.") || strings.HasPrefix(u, "//") || strings.HasPrefix(u, `\\`):
		return TrustWeb
	case toolTypes[st]:
		return TrustTool
	}
	return ""
}

// Untrusted reports whether content of this tier is third-party text: it is
// labelled untrusted, and prompt context can leave it out.
func (t Trust) Untrusted() bool { return t == TrustWeb || t == TrustTool || t == TrustChat }

// Label names the tier for a reader.
func (t Trust) Label() string {
	switch t {
	case TrustRepo:
		return "repo doc"
	case TrustSession:
		return "past session"
	case TrustFile:
		return "edited file"
	case TrustClient:
		return "stored by a client"
	case TrustWeb:
		return "web page, untrusted"
	case TrustTool:
		return "tool output, untrusted"
	case TrustChat:
		return "chat message, untrusted"
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
	// The raw pass reads the head of the text. The normalized pass first
	// drops what carries no words (format characters, markup, repeated
	// whitespace) from up to maxSqueeze bytes, so padding cannot push a
	// payload out of the maxScan bytes it then reads. Both are bounded, so
	// flagging a huge hit stays cheap; past maxSqueeze nothing is read, and
	// the warning is advisory either way.
	head := text
	if len(head) > maxScan {
		head = head[:maxScan]
	}
	return instructionPatterns.MatchString(head) || instructionPatterns.MatchString(normalize(squeeze(text)))
}

// maxScan is how many bytes of a text each pattern pass reads. A stored chunk
// is at most 126 tokens, well under this once padding is dropped.
const maxScan = 4 << 10

// maxSqueeze is how many bytes of a text squeeze reads.
const maxSqueeze = 1 << 20

// invisible reports whether r carries no visible word: a format or control
// character, a nonspacing mark (combining marks, variation selectors), or a
// blank filler letter.
func invisible(r rune) bool {
	switch r {
	case '\u115F', '\u1160', '\u3164', '\uFFA0', '\u2800':
		return true
	}
	return unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Mn, r)
}

// squeeze returns up to maxScan bytes of text read from its first maxSqueeze
// bytes, without invisible characters or markup and with each whitespace
// run cut to one space.
func squeeze(text string) string {
	if len(text) > maxSqueeze {
		text = text[:maxSqueeze]
	}
	var b strings.Builder
	space := false
	for _, r := range text {
		switch {
		case unicode.IsSpace(r):
			if space {
				continue
			}
			space, r = true, ' '
		case invisible(r) || strings.ContainsRune("*_`~>#|", r):
			continue
		default:
			space = false
		}
		b.WriteRune(r)
		if b.Len() >= maxScan {
			break
		}
	}
	return b.String()
}

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
