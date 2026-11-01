package memctx

import (
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/willesq/orchestrator/internal/bm25"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// Why a prompt or a hit was turned away, as `orch rag why` shows it.
const (
	ReasonHarnessPrompt = "written by the harness, not the user"
	ReasonNoTopic       = "names no topic memory could help with"
	ReasonHarnessText   = "harness text, not knowledge"
	ReasonTemporary     = "a temporary copy"
	ReasonInstructions  = "agent instructions the harness already loads"
	ReasonNoEvidence    = "shares too little with the prompt"
	ReasonLineOffTopic  = "its best line shows nothing of the prompt's topic"
	ReasonWeakerThanTop = "shows far less of the prompt than the best hit"
	ReasonQuotesRequest = "quotes an earlier request, not what came of it"
)

// minTopicTerms is how many topic terms a prompt needs to be worth a search, and
// a hit needs to share with it. One word in common is usually coincidence.
const minTopicTerms = 2

// strongMatch is the cosine at which meaning alone is evidence, with one shared
// term. Below it, MiniLM rates unrelated text of the same register this high.
const strongMatch = 0.6

// longPrompt is the number of topic terms past which a prompt, usually a pasted
// log or brief, shares words with almost anything, so a hit must share more.
const longPrompt = 40

// curatedFloor is the score a curated note must reach when it falls below the
// prompt's floor.
const curatedFloor = 0.35

// maxGateChars bounds the prompt text read, like the search's identifier scan.
const maxGateChars = 8192

// PromptCheck is the verdict on a prompt: whether to search for it, and the
// terms a hit needs evidence from.
type PromptCheck struct {
	Search bool
	Reason string // why not, when Search is false
	Terms  map[string]bool
	// Query is the prompt as read, cut to maxGateChars: the text hit lines are
	// picked against, so the line checked is the line shown.
	Query  string
	prompt string // Query in lower case
}

// harnessPrefixes open the turns a harness submits itself, such as a subagent
// handing back. Their wrapper text would match earlier copies of itself.
var harnessPrefixes = []string{
	"<agent-message", "<task-notification", "<system-reminder", "[subagent hand-back]", "[system notification",
	"<local-command-", "this session is being continued",
}

// recallPhrases ask about earlier work in words too plain to be a topic.
var recallPhrases = []string{
	"left off", "worked on", "working on", "we decide", "we decided", "last time", "last session", "remind me",
	"do we have", "don't we have", "what have we", "have we already", "remember when", "earlier session",
}

// CheckPrompt decides whether a prompt can use memory at all. A follow-up like
// "let's merge it" gets its meaning from the live session, so anything a search
// returned would only resemble its words.
func CheckPrompt(prompt string) PromptCheck {
	p := strings.TrimSpace(prompt)
	if len(p) > maxGateChars {
		p = strings.ToValidUTF8(p[:maxGateChars], "")
	}
	p = shellPrompt.ReplaceAllString(p, "")
	low := strings.ToLower(p)
	for _, w := range harnessPrefixes {
		if strings.HasPrefix(low, w) {
			return PromptCheck{Reason: ReasonHarnessPrompt}
		}
	}
	terms := topicTerms(p)
	if countStems(terms) >= minTopicTerms || hasIdentifier(p) {
		return PromptCheck{Search: true, Terms: terms, Query: p, prompt: low}
	}
	for _, w := range recallPhrases {
		if strings.Contains(low, w) {
			return PromptCheck{Search: true, Terms: terms, Query: p, prompt: low}
		}
	}
	return PromptCheck{Reason: ReasonNoTopic, Terms: terms, Query: p, prompt: low}
}

// shellPrompt matches the user@host part of a pasted terminal line, whose words
// appear in every path and log the user has ever stored.
var shellPrompt = regexp.MustCompile(`(?m)^[\w.-]+@[\w.-]+(?:\s+\S+)?\s+[%$#](?:\s|$)`)

// countStems counts terms, taking "cache" and "caching" as one piece of evidence.
func countStems(terms map[string]bool) int {
	stems := map[string]bool{}
	for t := range terms {
		stems[stem(t)] = true
	}
	return len(stems)
}

func stem(t string) string {
	for _, suf := range []string{"ing", "ed", "es", "s", "e"} {
		if r := strings.TrimSuffix(t, suf); r != t && len(r) >= 3 {
			return r
		}
	}
	return t
}

// CheckHit decides whether a hit that cleared the score floor earned its slot.
// An empty reason means it did.
func (p PromptCheck) CheckHit(h *orchdv1.SearchHit) string {
	uri := h.GetSourceUri()
	switch {
	case harnessText(h):
		return ReasonHarnessText
	case temporarySource(uri):
		return ReasonTemporary
	case instructionFile(h) && !p.names(uri):
		return ReasonInstructions
	}
	untrusted := TrustOf(h.GetTrust(), h.GetSourceType(), uri).Untrusted()
	// An identifier is evidence enough in anything but a page listing common names.
	if h.GetExact() && !untrusted {
		return ""
	}
	shared := p.sharedCount(h)
	need := minTopicTerms
	switch {
	case Curated(h) && !Flagged(h):
		need = 1
	case untrusted || len(p.Terms) > longPrompt:
		need = minTopicTerms + 1
	}
	if shared < need && !(shared >= 1 && h.GetScore() >= strongMatch) {
		return ReasonNoEvidence
	}
	// The line is all the agent sees. If it shows nothing of the prompt's topic the
	// match was somewhere else in the chunk, and the pointer would not explain itself.
	line := BestLine(p.Query, h.GetText(), h.GetSourceType())
	if !p.lineShows(line) {
		return ReasonLineOffTopic
	}
	// What was asked last time is not what came of it, and a re-pasted brief
	// matches its earlier copies better than anything that answered it.
	if h.GetSourceType() == "session_transcript" && userTurn.MatchString(line) {
		return ReasonQuotesRequest
	}
	return ""
}

// userTurn matches a transcript line opening a turn by the user, dated or not.
var userTurn = regexp.MustCompile(`^(?:\[[^\]]*\]\s*)?\[?user\]?:?\s`)

// PassesLowerFloor reports a hit worth keeping though it scored below the prompt's
// floor: a statement of what to do scores lower against a note that decided it
// than a question would, and curated notes are few. A client can name its note
// decision://, so this only widens what can surface, under its honest label, and
// a note that reads like an instruction gets no such help.
func (p PromptCheck) PassesLowerFloor(h *orchdv1.SearchHit) bool {
	return Curated(h) && !Flagged(h) && h.GetScore() >= curatedFloor && p.sharedCount(h) >= 1
}

// curatedSchemes and curatedTypes mark what someone chose to remember, as opposed
// to what was stored because it passed by.
var (
	curatedSchemes = []string{"decision://", "feedback://", "project://", "user://"}
	curatedTypes   = map[string]bool{"decision": true, "auto_memory": true}
)

// Curated reports a hit stored on purpose as a decision or a lesson.
func Curated(h *orchdv1.SearchHit) bool {
	// A note's URI is the writer's claim, so it cannot vouch for a web page.
	if TrustOf(h.GetTrust(), h.GetSourceType(), h.GetSourceUri()).Untrusted() {
		return false
	}
	if curatedTypes[h.GetSourceType()] {
		return true
	}
	for _, s := range curatedSchemes {
		if strings.HasPrefix(h.GetSourceUri(), s) {
			return true
		}
	}
	return false
}

// Weak reports a hit showing less than half the prompt's topics the best kept
// hit showed. A slot it would fill is better left empty.
func (p PromptCheck) Weak(h *orchdv1.SearchHit, top int) bool {
	return p.sharedCount(h)*2 < top
}

// Rank places a hit by how far its writer can be believed, ahead of any measure
// of its text: curated notes, then the rest, then untrusted pages, so that a page
// stuffed with a project's words cannot take a slot from, or set the bar for, what
// the user's own memory holds.
func Rank(h *orchdv1.SearchHit) int {
	switch {
	case TrustOf(h.GetTrust(), h.GetSourceType(), h.GetSourceUri()).Untrusted():
		return 2
	case Curated(h):
		return 0
	}
	return 1
}

// ByEvidence orders hits by how many of the prompt's topics each shows, keeping
// search order among equals, and reports the count for each hit. Fusion ranks by
// resemblance; with a slot at stake, covering the prompt matters more.
func (p PromptCheck) ByEvidence(hits []*orchdv1.SearchHit) map[*orchdv1.SearchHit]int {
	counts := make(map[*orchdv1.SearchHit]int, len(hits))
	for _, h := range hits {
		counts[h] = p.sharedCount(h)
	}
	sort.SliceStable(hits, func(i, j int) bool { return counts[hits[i]] > counts[hits[j]] })
	return counts
}

func (p PromptCheck) sharedCount(h *orchdv1.SearchHit) int {
	stems := map[string]bool{}
	for _, t := range p.Shared(h) {
		stems[stem(t)] = true
	}
	return len(stems)
}

func (p PromptCheck) lineShows(line string) bool {
	for _, t := range bm25.Tokens(line) {
		if p.Terms[t] {
			return true
		}
	}
	return false
}

// Shared lists the prompt's topic terms a hit contains in its text or heading. Not
// its URI: the writer chooses that freely, to match whatever it likes.
func (p PromptCheck) Shared(h *orchdv1.SearchHit) []string {
	in := map[string]bool{}
	for _, part := range []string{h.GetText(), h.GetHeading()} {
		for _, t := range bm25.Tokens(part) {
			in[t] = true
		}
	}
	var out []string
	for t := range p.Terms {
		if in[t] {
			out = append(out, t)
		}
	}
	return out
}

// names reports whether the prompt mentions a source's file by name, the one
// case where pointing at an instruction file is what was asked for.
func (p PromptCheck) names(uri string) bool {
	base := strings.ToLower(uri[strings.LastIndex(uri, "/")+1:])
	return base != "" && strings.Contains(p.prompt, base)
}

// NormalLine folds a line for comparing what two hits say.
func NormalLine(line string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(line), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
}

func topicTerms(text string) map[string]bool {
	out := map[string]bool{}
	for _, t := range bm25.Tokens(text) {
		if !genericTerms[t] && !digitsOnly(t) {
			out[t] = true
		}
	}
	return out
}

func digitsOnly(s string) bool {
	return strings.Trim(s, "0123456789") == ""
}

// identifierShape spots a name no conversational word looks like: a dotted,
// snake_case, camelCase or path-like token, or a quoted command.
var identifierShape = regexp.MustCompile("[A-Za-z0-9]+[_./][A-Za-z0-9_./-]*[A-Za-z0-9]|[a-z]+[A-Z][A-Za-z0-9]*|`[^`\n]+`")

func hasIdentifier(s string) bool { return identifierShape.MatchString(s) }

// sessionTypes hold whatever a session's harness put in its transcript. Only
// these are read for harness text: a repo doc may well describe it.
var sessionTypes = map[string]bool{"session_transcript": true, "session_record": true}

// harnessMarkers appear in context a harness assembled, not in anything said.
// Transcripts stored before ParseTranscript dropped that context still hold it.
var harnessMarkers = []string{
	"<system-reminder", "</system-reminder", "contents of /users/", "## auto-injected rag context", "## orchestrator digest",
	"[subagent hand-back]", "<task-notification", "<agent-message", "userpromptsubmit hook", "sessionstart hook",
	"available agent types", "the following skills are available", "the following deferred tools",
	"mcp server instructions", "primary working directory", "attribution for git commits",
}

// harnessHeadings are the sections that context parses into.
var harnessHeadings = []string{
	"mcp server instructions", "shared coding-agent expectations", "environment", "gitstatus", "computer use",
	"claude.ai claude docs", "serena", "claude-in-chrome", "load the version-matched guide",
}

func harnessText(h *orchdv1.SearchHit) bool {
	if !sessionTypes[h.GetSourceType()] {
		return false
	}
	heading := strings.ToLower(h.GetHeading())
	for _, m := range harnessHeadings {
		if heading == m || strings.HasPrefix(heading, m+" ›") || strings.HasSuffix(heading, "› "+m) {
			return true
		}
	}
	text := strings.ToLower(h.GetText())
	for _, m := range harnessMarkers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// temporaryPrefixes hold scratch files. With worktree and workspace checkouts,
// these are throwaway copies: the real document, when there is one, is stored
// under its own name.
var temporaryPrefixes = []string{"file:///private/tmp/", "file:///tmp/", "file:///private/var/folders/", "file:///var/folders/"}

func temporarySource(uri string) bool {
	for _, p := range temporaryPrefixes {
		if strings.HasPrefix(uri, p) {
			return true
		}
	}
	return strings.HasPrefix(uri, "file://") && (strings.Contains(uri, ".worktrees/") || strings.Contains(uri, "/orca/workspaces/"))
}

// instructionFile reports a source that is itself agent instructions. These
// match prompts by the trigger phrases they list, and the agent already has them
// or loads them on demand, so a pointer tells it nothing new.
func instructionFile(h *orchdv1.SearchHit) bool {
	if h.GetSourceType() == "claude_config" {
		return true
	}
	uri := h.GetSourceUri()
	base := uri[strings.LastIndex(uri, "/")+1:]
	switch base {
	case "SKILL.md", "AGENTS.md", "CLAUDE.md", "GEMINI.md":
		return true
	}
	return strings.Contains(uri, "/internal/install/")
}

// genericTerms carry no topic in a coding session: conversation, and the verbs
// of the work itself. "Merge the PR" says what to do now, not what to recall.
var genericTerms = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`about above actually after again ahead all allow almost already alright also always am
		any anything around ask at aspect away back based because been before being below best better between
		big both bring change changes could currently did didn doesn don done down each either else even ever every
		everything example few fine first follow following forward fully get getting give given go going good got
		great guess had hasn haven having help here hey hi hmm just keep kind know last later least left less let
		lets like likely little ll long look looking looks lot made maybe me mean might mine more most much must my
		need needs new next nice no nope not now of off ok okay one only other others our out over own part
		please pretty probably put quite rather re really right said same say says see seem seems should sh shouldn
		since small some something sorry start started still stuff such sure take taking tell thank thanks that's them
		then there's thing things think this those though through time times today too tried try trying two uh um under
		until up us use used using ve very via wait want wanted wants way well went were weren what's whatever while
		why would wouldn yeah yep yes yet you'd
		add added approve approved branch build check checks ci close code commit commits complete completed create
		created cut deploy failed failing fail fails file files finish fix fixed issue issues job jobs lgtm make
		makes making merge merged merges merging open passed passes pr prs proceed progress pull push pushed ready
		release released remove removed repo repository review reviewed reviews run running runs ship test tests
		update updated work worked working works`) {
		m[w] = true
	}
	return m
}()
