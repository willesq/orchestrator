package config

import (
	"slices"
	"strings"
	"testing"
)

// TestChatFeedSettings: the chat allowlist stores nothing by default, is
// sensitive, and the operator's values reach Settings (SP-060).
func TestChatFeedSettings(t *testing.T) {
	s, err := Unmarshal(loadTemp(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Feeds.Chat.DirectMessages) != 0 || len(s.Feeds.Chat.Containers) != 0 {
		t.Fatalf("default chat allowlist = %+v, want empty", s.Feeds.Chat)
	}
	for _, key := range []string{"feeds.chat.direct_messages", "feeds.chat.containers"} {
		if !mustField(t, key).Sensitive {
			t.Errorf("%s is not sensitive", key)
		}
	}
	if err := applyTemp(t, map[string]any{"feeds.chat.direct_messages": []string{"slack:T0123"}}, false); err == nil {
		t.Fatal("a sensitive chat setting was stored without confirmation")
	}
	if err := applyTemp(t, map[string]any{
		"feeds.chat.direct_messages": []string{"slack:T0123"},
		"feeds.chat.containers":      map[string]any{"slack:T0123": []string{"C0456", "G0789"}},
	}, true); err != nil {
		t.Fatal(err)
	}
	if s, err = Unmarshal(reload(t)); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.Feeds.Chat.DirectMessages, []string{"slack:T0123"}) {
		t.Errorf("direct_messages = %v", s.Feeds.Chat.DirectMessages)
	}
	// Keys are folded to lower case by the loader; tenants match
	// regardless of case (feeds.Allowlist). Container IDs keep theirs.
	for tenant, ids := range s.Feeds.Chat.Containers {
		if !strings.EqualFold(tenant, "slack:T0123") || !slices.Equal(ids, []string{"C0456", "G0789"}) {
			t.Errorf("containers = %v", s.Feeds.Chat.Containers)
		}
	}
	if len(s.Feeds.Chat.Containers) != 1 {
		t.Errorf("containers = %v", s.Feeds.Chat.Containers)
	}
}
