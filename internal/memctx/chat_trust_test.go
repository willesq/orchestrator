package memctx

import (
	"strings"
	"testing"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// TestChatIsUntrusted: chat a chat feed stored is third-party text (SP-060):
// its tier is chat whatever its type or URI, it is labelled untrusted, and
// prompt context leaves it out under untrusted: exclude.
func TestChatIsUntrusted(t *testing.T) {
	for _, c := range []struct{ sourceType, uri string }{
		{"chat", "slack://T1/C1/1700000000.000100"},
		{"repo_doc", "repo://a/b/c"}, // a declared type never raises it
		{"chat", "https://x.slack.com/archives/C1"},
	} {
		if got := TrustOf("chat", c.sourceType, c.uri); got != TrustChat {
			t.Errorf("TrustOf(chat, %q, %q) = %q", c.sourceType, c.uri, got)
		}
	}
	if !TrustChat.Untrusted() || !strings.Contains(TrustChat.Label(), "untrusted") {
		t.Errorf("chat: untrusted %v, label %q", TrustChat.Untrusted(), TrustChat.Label())
	}
	hits := []*orchdv1.SearchHit{
		{Text: "a channel message", SourceUri: "slack://T1/C1", SourceType: "chat", Trust: "chat", Score: 0.9},
		{Text: "a repo doc", SourceUri: "repo://a/b/c", SourceType: "repo_doc", Trust: "repo", Score: 0.8},
	}
	text, written := Render("q", hits, Options{Untrusted: UntrustedExclude})
	if len(written) != 1 || strings.Contains(text, "slack://") {
		t.Errorf("exclude kept chat: %q", text)
	}
	text, _ = Render("q", hits, Options{Format: FormatFull})
	if !strings.Contains(text, "chat message, untrusted") {
		t.Errorf("chat not labelled untrusted: %q", text)
	}
}
