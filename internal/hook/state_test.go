package hook

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"

	"github.com/willesq/orchestrator/internal/paths"
)

func TestStateRoundTripAndCap(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	st := sessionState{}
	for i := range maxInjected + 10 {
		st.Injected = append(st.Injected, hitKey("u", strings.Repeat("x", i)))
	}
	if err := saveState("sid-1", st); err != nil {
		t.Fatalf("saveState: %v", err)
	}
	got := loadState("sid-1")
	if len(got.Injected) != maxInjected || got.Injected[0] != st.Injected[10] {
		t.Errorf("loaded %d keys (first %q), want the newest %d", len(got.Injected), got.Injected[0], maxInjected)
	}
	p, _ := statePath("sid-1")
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("state mode = %v, want 0600", info.Mode().Perm())
	}
	deleteState("sid-1")
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("state survived deleteState (err=%v)", err)
	}
}

func TestStateRejectsUnsafeSessionIDs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	t.Setenv("ORCH_DATA_DIR", dir)
	for _, id := range []string{"", "../escape", "a/b", "x.y", strings.Repeat("a", 129)} {
		if _, ok := statePath(id); ok {
			t.Errorf("statePath(%q) accepted an unsafe id", id)
		}
		if err := saveState(id, sessionState{Injected: []string{"k"}}); err != nil {
			t.Errorf("saveState(%q) = %v, want a silent no-op", id, err)
		}
	}
	if entries, _ := os.ReadDir(filepath.Dir(dir)); len(entries) > 1 {
		t.Errorf("unsafe ids wrote files: %v", entries)
	}
	if got := loadState("../escape"); len(got.Injected) != 0 {
		t.Errorf("loadState read through an unsafe id")
	}
}

func TestUserPromptSubmitDoesNotRepeatHits(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	fc := &fakeClient{search: &orchdv1.SearchResponse{Hits: []*orchdv1.SearchHit{
		{SourceUri: "manual://a", Text: "alpha", Score: 0.9},
		{SourceUri: "manual://b", Text: "beta", Score: 0.9},
	}}}
	cfg := defaultAutoRAG()
	cfg.TopK = 1
	prompt := func() string {
		var out bytes.Buffer
		if err := userPromptSubmit(cfg, Payload{SessionID: "sid", Prompt: "how do retries work?"}, &out, factory(fc, nil)); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	first, second, third := prompt(), prompt(), prompt()
	if !strings.Contains(first, "manual://a") {
		t.Fatalf("first turn missing the top hit:\n%s", first)
	}
	if strings.Contains(second, "manual://a") || !strings.Contains(second, "manual://b") {
		t.Errorf("second turn should move on to the next hit:\n%s", second)
	}
	if third != "" {
		t.Errorf("third turn re-injected already shown hits:\n%s", third)
	}
	// A different session starts fresh.
	var out bytes.Buffer
	_ = userPromptSubmit(cfg, Payload{SessionID: "other", Prompt: "how do retries work?"}, &out, factory(fc, nil))
	if !strings.Contains(out.String(), "manual://a") {
		t.Errorf("another session did not get the hit")
	}
	if _, err := os.Stat(filepath.Join(paths.SessionsDir(), "sid.hook.json")); err != nil {
		t.Errorf("state not persisted: %v", err)
	}
}

func TestSessionEndRemovesHookState(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	if err := saveState("sid", sessionState{Injected: []string{"k"}}); err != nil {
		t.Fatal(err)
	}
	if err := sessionEnd(Payload{SessionID: "sid", TranscriptPath: writeSampleTranscript(t)}, factory(&fakeClient{}, nil)); err != nil {
		t.Fatal(err)
	}
	if p, _ := statePath("sid"); fileExists(p) {
		t.Errorf("hook state outlived the session")
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func TestStateDegradesOnBadFiles(t *testing.T) {
	t.Run("corrupt state reads as empty", func(t *testing.T) {
		t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
		p, _ := statePath("sid")
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := loadState("sid"); len(got.Injected) != 0 {
			t.Errorf("corrupt state produced %v", got.Injected)
		}
	})
	t.Run("unwritable data dir is an error, never a panic", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(blocker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ORCH_DATA_DIR", blocker) // a file where the data dir should be
		if err := saveState("sid", sessionState{Injected: []string{"k"}}); err == nil {
			t.Errorf("saveState succeeded under a file")
		}
	})
	t.Run("a prompt still injects when state cannot be saved", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(blocker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ORCH_DATA_DIR", blocker)
		var out bytes.Buffer
		err := userPromptSubmit(defaultAutoRAG(), Payload{SessionID: "sid", Prompt: "how do retries work?"}, &out, factory(&fakeClient{search: hitsResp(0.9)}, nil))
		if err != nil || !strings.Contains(out.String(), "manual://x") {
			t.Errorf("err=%v out=%q", err, out.String())
		}
	})
}
