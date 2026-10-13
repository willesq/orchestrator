package hook

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/session"
)

const compactTranscript = `{"type":"user","message":{"role":"user","content":"add retries to the uploader"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Added exponential backoff in uploader.go; tests pass."}]}}
{"type":"user","message":{"role":"user","content":"<local-command-stdout>ok</local-command-stdout>"}}
`

func writeTranscript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResumeBriefAfterCompaction(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	repo := newRepo(t, "widget", "")
	if err := os.WriteFile(filepath.Join(repo, "uploader.go"), []byte("package x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := Payload{SessionID: "sid", Cwd: repo, TranscriptPath: writeTranscript(t, compactTranscript)}
	if err := preCompact(p); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		source  string
		want    []string
		wantNot string
	}{
		{name: "compaction gets the brief", source: "compact", want: []string{
			"## Resume brief", "- Working directory: " + repo, "- Branch: main",
			"- Uncommitted changes (1 files): uploader.go", "- Recent commits: ",
			"- Last request: add retries to the uploader",
			"- Last reply (excerpt): Added exponential backoff in uploader.go; tests pass.",
		}},
		{name: "a fresh start gets none", source: "startup", wantNot: "Resume brief"},
		{name: "a resumed session gets none", source: "resume", wantNot: "Resume brief"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			p.Source = tt.source
			if err := sessionStart(p, &out); err != nil {
				t.Fatal(err)
			}
			got := out.String()
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("brief missing %q:\n%s", w, got)
				}
			}
			if tt.wantNot != "" && strings.Contains(got, tt.wantNot) {
				t.Errorf("unexpected brief:\n%s", got)
			}
		})
	}
}

func TestResumeBriefDegrades(t *testing.T) {
	t.Run("outside a repository", func(t *testing.T) {
		t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
		isolateGit(t)
		dir := t.TempDir()
		var out bytes.Buffer
		writeResumeBrief(&out, Payload{SessionID: "sid", Cwd: dir})
		got := out.String()
		if !strings.Contains(got, "- Working directory: "+dir) || strings.Contains(got, "Branch") {
			t.Errorf("got:\n%s", got)
		}
	})
	t.Run("nothing to say prints nothing", func(t *testing.T) {
		t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
		var out bytes.Buffer
		writeResumeBrief(&out, Payload{SessionID: "sid"})
		if out.Len() != 0 {
			t.Errorf("got %q", out.String())
		}
	})
	t.Run("an unsafe session ID never reads a file", func(t *testing.T) {
		data := filepath.Join(t.TempDir(), "data")
		t.Setenv("ORCH_DATA_DIR", data)
		// A checkpoint planted where "../x" would resolve must not be read.
		if err := session.WriteCheckpoint(filepath.Dir(paths.SessionsDir()), &session.Record{SessionID: "x", LastRequest: "planted"}); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		writeResumeBrief(&out, Payload{SessionID: "../x"})
		if strings.Contains(out.String(), "planted") {
			t.Errorf("read through an unsafe session ID:\n%s", out.String())
		}
	})
	t.Run("many uncommitted files are summarized", func(t *testing.T) {
		repo := newRepo(t, "big", "")
		for i := range maxBriefFiles + 3 {
			if err := os.WriteFile(filepath.Join(repo, strings.Repeat("f", i+1)), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		lines := gitState(repo)
		joined := strings.Join(lines, "\n")
		if !strings.Contains(joined, "(13 files)") || !strings.HasSuffix(strings.TrimSpace(strings.Split(joined, "\n")[1]), "…") {
			t.Errorf("got:\n%s", joined)
		}
	})
}
